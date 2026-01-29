package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

// Modelo Proyecto: Guarda la imagen en BINARIO (Byte Array) para persistencia en Railway
type Proyecto struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Titulo    string `gorm:"type:varchar(100)" json:"titulo"`
	Categoria string `gorm:"type:varchar(50)" json:"categoria"`

	// DATOS BINARIOS DE LA IMAGEN
	Datos    []byte `gorm:"type:bytea" json:"-"`              // json:"-" para no enviarlo en la lista
	MimeType string `gorm:"type:varchar(50)" json:"mimeType"` // ej: image/jpeg

	CreatedAt time.Time `json:"fecha"`
}

// Estructura ligera para enviar la lista al frontend (sin los bytes pesados)
type ProyectoResponse struct {
	ID        uint      `json:"id"`
	Titulo    string    `json:"titulo"`
	Categoria string    `json:"categoria"`
	CreatedAt time.Time `json:"fecha"`
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
// 2. CONFIGURACIÓN
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
// 3. MAIN
// ---------------------------------------------------------

func main() {
	// Cargar variables (opcional si usas Railway Variables)
	if err := godotenv.Load(); err != nil {
		fmt.Println("ℹ️ Nota: Usando variables de entorno del sistema.")
	}

	// Conexión DB
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("❌ Error: DB_DSN está vacía.")
	}

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Falló conexión a DB: ", err)
	}
	fmt.Println("✅ Conectado a PostgreSQL")

	// Migraciones
	db.AutoMigrate(&ContactoWeb{}, &Proyecto{})

	e := echo.New()
	e.Validator = &CustomValidator{validator: validator.New()}

	// MIDDLEWARES
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.Secure())
	e.Use(middleware.BodyLimit("5M")) // Permitir subidas de hasta 5MB

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
	e.Use(middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Skipper: middleware.DefaultSkipper,
		Store:   middleware.NewRateLimiterMemoryStore(rate.Limit(5)),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			return ctx.RealIP(), nil
		},
	}))

	// RUTAS
	e.Static("/", "public")

	// API Rutas
	e.POST("/api/contacto", manejarContacto)

	// --- RUTAS DE IMÁGENES (SISTEMA DE BD) ---
	e.POST("/api/upload", subirProyectoBD)      // Subir bytes
	e.GET("/api/proyectos", obtenerProyectosBD) // Obtener lista JSON
	e.GET("/api/imagen/:id", servirImagenBD)    // Obtener la imagen visual

	// Puerto
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	fmt.Println("🚀 Servidor corriendo en " + port)
	e.Logger.Fatal(e.Start(port))
}

// ---------------------------------------------------------
// 4. HANDLERS
// ---------------------------------------------------------

// A. Subir Imagen a Base de Datos
func subirProyectoBD(c echo.Context) error {
	// 1. Obtener archivo
	file, err := c.FormFile("imagen")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No se envió imagen"})
	}

	// 2. Validar extensión
	ext := strings.ToLower(file.Filename)
	if !strings.HasSuffix(ext, ".jpg") && !strings.HasSuffix(ext, ".jpeg") && !strings.HasSuffix(ext, ".png") {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Solo formato JPG o PNG"})
	}

	// 3. Abrir
	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al procesar archivo"})
	}
	defer src.Close()

	// 4. Leer BYTES (Esto es lo que guardaremos en Postgres)
	fileBytes, err := io.ReadAll(src)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al leer bytes"})
	}

	// 5. Guardar en DB
	nuevoProyecto := Proyecto{
		Titulo:    "Diseño de Comunidad",
		Categoria: "Upload",
		Datos:     fileBytes, // Guardamos el binario
		MimeType:  file.Header.Get("Content-Type"),
		CreatedAt: time.Now(),
	}

	if result := db.Create(&nuevoProyecto); result.Error != nil {
		fmt.Println("Error DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error guardando en base de datos"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Imagen guardada correctamente"})
}

// B. Obtener Lista de Proyectos (Ligera)
func obtenerProyectosBD(c echo.Context) error {
	var proyectos []Proyecto
	// Seleccionamos solo campos necesarios (EXCLUYENDO 'Datos' que es pesado)
	if result := db.Select("id", "titulo", "categoria", "created_at").Order("created_at desc").Limit(20).Find(&proyectos); result.Error != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al obtener lista"})
	}

	// Mapear a respuesta limpia
	var response []ProyectoResponse
	for _, p := range proyectos {
		response = append(response, ProyectoResponse{
			ID:        p.ID,
			Titulo:    p.Titulo,
			Categoria: p.Categoria,
			CreatedAt: p.CreatedAt,
		})
	}

	return c.JSON(http.StatusOK, response)
}

// C. Servir la Imagen Visual (Src del <img>)
func servirImagenBD(c echo.Context) error {
	id := c.Param("id")
	var proy Proyecto

	// Buscamos el registro completo (incluyendo los bytes)
	if result := db.First(&proy, id); result.Error != nil {
		return c.NoContent(http.StatusNotFound)
	}

	// Escribimos los bytes como respuesta con el MimeType correcto
	return c.Blob(http.StatusOK, proy.MimeType, proy.Datos)
}

func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
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
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error DB"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Enviado"})
}

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if secret == "" {
		return false // Falla segura
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
