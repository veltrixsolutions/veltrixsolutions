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
// 1. MODELOS DE DATOS (STRUCTS)
// ---------------------------------------------------------

// Proyecto representa las imágenes de la galería/carrusel
type Proyecto struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Titulo      string    `gorm:"type:varchar(100)" json:"titulo"`
	Categoria   string    `gorm:"type:varchar(50)" json:"categoria"`
	Descripcion string    `gorm:"type:text" json:"descripcion"`
	ImagenURL   string    `gorm:"type:text" json:"imagenUrl"` // Ruta pública (ej: /uploads/foto.jpg)
	CreatedAt   time.Time `json:"fecha"`
}

// ContactoWeb representa los mensajes del formulario
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

// ContactoRequest valida los datos que llegan del frontend
type ContactoRequest struct {
	Nombre         string `json:"nombre" validate:"required,min=2,max=100"`
	Email          string `json:"email" validate:"required,email,max=150"`
	Telefono       string `json:"telefono" validate:"omitempty,numeric,max=20"`
	Servicio       string `json:"servicio" validate:"max=50"`
	Mensaje        string `json:"mensaje" validate:"required,min=10,max=2000"`
	RecaptchaToken string `json:"recaptchaToken" validate:"required"`
}

// RecaptchaResponse mapea la respuesta de Google
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

// Variable global para la DB
var db *gorm.DB

// ---------------------------------------------------------
// 3. FUNCIÓN MAIN (Punto de Entrada)
// ---------------------------------------------------------

func main() {
	// A. Cargar variables de entorno
	if err := godotenv.Load(); err != nil {
		fmt.Println("ℹ️ Nota: No se encontró .env, usando variables del sistema.")
	}

	// B. Conexión a Base de Datos
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

	// Migración automática
	db.AutoMigrate(&ContactoWeb{}, &Proyecto{})

	// C. Inicializar Echo
	e := echo.New()
	e.Validator = &CustomValidator{validator: validator.New()}

	// ---------------------------------------------------------
	// 4. MIDDLEWARES
	// ---------------------------------------------------------

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.Secure())
	e.Use(middleware.BodyLimit("5M")) // Aumentado a 5MB para permitir imágenes

	// CORS
	allowOrigin := os.Getenv("FRONTEND_URL")
	if allowOrigin == "" {
		allowOrigin = "*"
	}
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{allowOrigin},
		AllowMethods: []string{http.MethodPost, http.MethodGet},
	}))

	// Rate Limiter
	configRateLimit := middleware.RateLimiterConfig{
		Skipper: middleware.DefaultSkipper,
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(
			middleware.RateLimiterMemoryStoreConfig{Rate: rate.Limit(5), Burst: 10, ExpiresIn: 3 * time.Minute},
		),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			return ctx.RealIP(), nil
		},
		ErrorHandler: func(context echo.Context, err error) error {
			return context.JSON(http.StatusTooManyRequests, map[string]string{"error": "Demasiadas peticiones."})
		},
	}
	e.Use(middleware.RateLimiterWithConfig(configRateLimit))

	// ---------------------------------------------------------
	// 5. RUTAS Y ARCHIVOS ESTÁTICOS
	// ---------------------------------------------------------

	// Servir index.html y assets (css, js)
	e.Static("/", "public")
	// IMPORTANTE: Servir la carpeta de subidas para que las imágenes sean visibles
	e.Static("/uploads", "public/uploads")

	// API Endpoints
	e.POST("/api/contacto", manejarContacto)
	e.POST("/api/upload", subirProyecto)      // Subir imagen para galería
	e.GET("/api/proyectos", obtenerProyectos) // Leer imágenes para el carrusel

	// ---------------------------------------------------------
	// 6. ARRANQUE
	// ---------------------------------------------------------

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
// 7. HANDLERS (Lógica)
// ---------------------------------------------------------

// --- A. CONTACTO ---
func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Formato inválido"})
	}
	if err := c.Validate(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Datos inválidos: " + err.Error()})
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
		fmt.Println("Error DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error interno"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Enviado correctamente"})
}

// --- B. SUBIR IMAGEN (Para la Galería) ---
func subirProyecto(c echo.Context) error {
	// 1. Leer archivo del form-data
	file, err := c.FormFile("imagen")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No se envió imagen"})
	}

	// 2. Validar extensión y tamaño
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Solo se permiten JPG o PNG"})
	}
	if file.Size > 2*1024*1024 { // 2MB
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Imagen muy pesada (Max 2MB)"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al abrir archivo"})
	}
	defer src.Close()

	// 3. Crear carpeta si no existe
	uploadDir := "public/uploads"
	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		os.MkdirAll(uploadDir, 0755)
	}

	// 4. Guardar archivo en disco con nombre único
	newFileName := fmt.Sprintf("%d%s", time.Now().Unix(), ext)
	dstPath := filepath.Join(uploadDir, newFileName)

	dst, err := os.Create(dstPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al guardar en disco"})
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al escribir archivo"})
	}

	// 5. Guardar referencia en Base de Datos
	// Construimos la URL pública para el frontend
	publicURL := fmt.Sprintf("/uploads/%s", newFileName)

	nuevoProyecto := Proyecto{
		Titulo:      "Diseño de Comunidad",
		Categoria:   "Upload",
		Descripcion: "Imagen subida por usuario",
		ImagenURL:   publicURL, // Guardamos la ruta, no los bytes
	}

	if result := db.Create(&nuevoProyecto); result.Error != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error DB"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Subido correctamente", "url": publicURL})
}

// --- C. OBTENER PROYECTOS (Para el Carrusel) ---
func obtenerProyectos(c echo.Context) error {
	var proyectos []Proyecto
	// Traemos los últimos 10 proyectos
	if result := db.Order("created_at desc").Limit(10).Find(&proyectos); result.Error != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al obtener datos"})
	}
	return c.JSON(http.StatusOK, proyectos)
}

// ---------------------------------------------------------
// 8. HELPERS
// ---------------------------------------------------------

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if secret == "" {
		fmt.Println("❌ Error: Falta RECAPTCHA_SECRET")
		return false
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm("https://www.google.com/recaptcha/api/siteverify", map[string][]string{
		"secret":   {secret},
		"response": {token},
	})
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var googleResult RecaptchaResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleResult); err != nil {
		return false
	}
	return googleResult.Success
}
