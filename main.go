package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// ---------------------------------------------------------
// 1. MODELOS DE DATOS
// ---------------------------------------------------------

type Proyecto struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Titulo      string    `gorm:"type:varchar(100)" json:"titulo"`
	Categoria   string    `gorm:"type:varchar(50)" json:"categoria"`
	Descripcion string    `gorm:"type:text" json:"descripcion"`
	ImagenURL   string    `gorm:"type:text" json:"imagenUrl"`
	CreatedAt   time.Time `json:"fecha"`
}

type ContactoWeb struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Nombre    string    `gorm:"type:varchar(100);not null" json:"nombre"`
	Email     string    `gorm:"type:varchar(150);not null" json:"email"`
	Telefono  string    `gorm:"type:varchar(20)" json:"telefono"`
	Servicio  string    `gorm:"type:varchar(50)" json:"servicio"`
	Mensaje   string    `gorm:"type:text" json:"mensaje"`
	IpUsuario string    `gorm:"type:varchar(50)" json:"-"`
	CreatedAt time.Time `json:"fecha"`
}

type ContactoRequest struct {
	Nombre         string `json:"nombre" validate:"required,min=2,max=100"`
	Email          string `json:"email" validate:"required,email,max=150"`
	Telefono       string `json:"telefono" validate:"omitempty,numeric,max=20"`
	Servicio       string `json:"servicio" validate:"max=50"`
	Mensaje        string `json:"mensaje" validate:"required,min=10,max=2000"`
	RecaptchaToken string `json:"recaptchaToken" validate:"required"`
}

type RecaptchaResponse struct {
	Success bool `json:"success"`
}

// ---------------------------------------------------------
// 2. CONFIGURACIÓN DEL VALIDADOR
// ---------------------------------------------------------

type CustomValidator struct {
	validator *validator.Validate
}

func (cv *CustomValidator) Validate(i interface{}) error {
	if err := cv.validator.Struct(i); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return nil
}

var db *gorm.DB

// ---------------------------------------------------------
// 3. FUNCIÓN MAIN
// ---------------------------------------------------------

func main() {
	if err := godotenv.Load(); err != nil {
		fmt.Println("ℹ️ Nota: No se encontró .env, usando variables del sistema.")
	}

	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("❌ Error crítico: La variable DB_DSN está vacía.")
	}

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Falló la conexión a la base de datos: ", err)
	}
	fmt.Println("✅ Conexión a PostgreSQL establecida.")

	db.AutoMigrate(&ContactoWeb{}, &Proyecto{})

	e := echo.New()
	e.Validator = &CustomValidator{validator: validator.New()}

	// MIDDLEWARES
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.Secure())
	e.Use(middleware.BodyLimit("5M"))

	allowOrigin := os.Getenv("FRONTEND_URL")
	if allowOrigin == "" {
		allowOrigin = "*"
	}
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{allowOrigin},
		AllowMethods: []string{http.MethodPost, http.MethodGet},
	}))

	e.Use(middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Skipper: middleware.DefaultSkipper,
		Store:   middleware.NewRateLimiterMemoryStore(rate.Limit(5)),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			return ctx.RealIP(), nil
		},
	}))

	// RUTAS Y ARCHIVOS ESTÁTICOS
	e.Static("/", "public")
	// CORRECCIÓN: Servir /uploads apuntando a la carpeta física public/uploads
	e.Static("/uploads", "public/uploads")

	e.POST("/api/contacto", manejarContacto)
	e.POST("/api/upload", subirProyecto)
	e.GET("/api/proyectos", obtenerProyectos)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	fmt.Println("🚀 Servidor Veltrix corriendo en " + port)
	e.Logger.Fatal(e.Start(port))
}

// ---------------------------------------------------------
// 4. HANDLERS
// ---------------------------------------------------------

func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Formato inválido"})
	}
	if err := c.Validate(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if !validarCaptchaGoogle(req.RecaptchaToken) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Captcha inválido"})
	}

	nuevoContacto := ContactoWeb{
		Nombre:    strings.TrimSpace(req.Nombre),
		Email:     strings.TrimSpace(req.Email),
		Telefono:  strings.TrimSpace(req.Telefono),
		Servicio:  strings.TrimSpace(req.Servicio),
		Mensaje:   strings.TrimSpace(req.Mensaje),
		IpUsuario: c.RealIP(),
	}

	if result := db.Create(&nuevoContacto); result.Error != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error interno"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Enviado correctamente"})
}

func subirProyecto(c echo.Context) error {
	file, err := c.FormFile("imagen")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No se envió imagen"})
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Solo JPG o PNG"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al abrir"})
	}
	defer src.Close()

	// Carpeta física
	uploadDir := filepath.Join("public", "uploads")
	os.MkdirAll(uploadDir, 0755)

	newFileName := fmt.Sprintf("%d%s", time.Now().Unix(), ext)
	dstPath := filepath.Join(uploadDir, newFileName)

	dst, err := os.Create(dstPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al crear archivo"})
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al escribir"})
	}

	// URL pública que guardamos en DB
	publicURL := fmt.Sprintf("/uploads/%s", newFileName)

	nuevoProyecto := Proyecto{
		Titulo:    "Diseño de Comunidad",
		Categoria: "Upload",
		ImagenURL: publicURL,
	}

	db.Create(&nuevoProyecto)

	return c.JSON(http.StatusCreated, map[string]string{"url": publicURL})
}

func obtenerProyectos(c echo.Context) error {
	var proyectos []Proyecto
	db.Order("created_at desc").Limit(10).Find(&proyectos)
	return c.JSON(http.StatusOK, proyectos)
}

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if secret == "" {
		return false
	}
	resp, err := http.PostForm("https://www.google.com/recaptcha/api/siteverify", map[string][]string{
		"secret":   {secret},
		"response": {token},
	})
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var result RecaptchaResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Success
}
