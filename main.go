package main

import (
	"encoding/json"
	"fmt"
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

// ---------------- MODELOS DE DATOS ----------------

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

// ContactoRequest incluye etiquetas 'validate' para reglas estrictas
type ContactoRequest struct {
	Nombre         string `json:"nombre" validate:"required,min=2,max=100"`
	Email          string `json:"email" validate:"required,email,max=150"`
	Telefono       string `json:"telefono" validate:"omitempty,max=20,numeric"` // Opcional, solo números
	Servicio       string `json:"servicio" validate:"max=50"`
	Mensaje        string `json:"mensaje" validate:"required,min=10,max=2000"` // Mínimo 10 caracteres para evitar spam vacío
	RecaptchaToken string `json:"recaptchaToken" validate:"required"`
}

type RecaptchaResponse struct {
	Success bool `json:"success"`
}

// ---------------- CONFIGURACIÓN DE VALIDACIÓN ----------------

type CustomValidator struct {
	validator *validator.Validate
}

func (cv *CustomValidator) Validate(i interface{}) error {
	if err := cv.validator.Struct(i); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return nil
}

// ---------------- VARIABLES GLOBALES ----------------

var db *gorm.DB

// ---------------- FUNCIÓN PRINCIPAL ----------------

func main() {
	if err := godotenv.Load(); err != nil {
		fmt.Println("ℹ️ Nota: Usando variables de entorno del sistema.")
	}

	// 1. Conexión a Base de Datos
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("❌ Error: DB_DSN está vacío.")
	}

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Falló la conexión a Railway: ", err)
	}
	fmt.Println("✅ Conectado exitosamente a PostgreSQL en Railway")

	// Migración automática
	db.AutoMigrate(&ContactoWeb{})

	// 2. Configuración de Echo
	e := echo.New()

	// Registrar el validador personalizado
	e.Validator = &CustomValidator{validator: validator.New()}

	// ---------------- MIDDLEWARES DE SEGURIDAD ----------------

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// A. Protección de Cabeceras (XSS, HSTS, Sniffing)
	e.Use(middleware.Secure())

	// B. Limite de tamaño del cuerpo (Evita ataques de carga masiva)
	// Limita a 2KB para un formulario de texto simple
	e.Use(middleware.BodyLimit("2K"))

	// C. CORS Estricto (Crucial para producción)
	allowOrigin := os.Getenv("FRONTEND_URL")
	if allowOrigin == "" {
		allowOrigin = "*" // Solo para desarrollo, advertir en logs
		fmt.Println("⚠️ ADVERTENCIA: CORS permitido para todo (*). Configura FRONTEND_URL en producción.")
	}

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{allowOrigin},
		AllowMethods: []string{http.MethodPost}, // Solo permitimos POST para esta API
	}))

	// D. Rate Limiter (Limita peticiones por IP)
	// Permite 5 peticiones por segundo con una ráfaga de 10 (ajustar según necesidad)
	// Para formularios de contacto, idealmente sería más lento (ej. 2 por minuto), pero este es un config general.
	configRateLimit := middleware.RateLimiterConfig{
		Skipper: middleware.DefaultSkipper,
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(
			middleware.RateLimiterMemoryStoreConfig{Rate: rate.Limit(5), Burst: 10, ExpiresIn: 3 * time.Minute},
		),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			return ctx.RealIP(), nil
		},
		ErrorHandler: func(context echo.Context, err error) error {
			return context.JSON(http.StatusTooManyRequests, map[string]string{"error": "Demasiadas peticiones. Intenta más tarde."})
		},
		DenyHandler: func(context echo.Context, identifier string, err error) error {
			return context.JSON(http.StatusTooManyRequests, map[string]string{"error": "Demasiadas peticiones. Intenta más tarde."})
		},
	}
	e.Use(middleware.RateLimiterWithConfig(configRateLimit))

	// ---------------- RUTAS ----------------

	e.Static("/", "public")
	e.POST("/api/contacto", manejarContacto)

	// ---------------- INICIO DEL SERVIDOR ----------------

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	fmt.Println("🚀 Servidor iniciando en " + port)
	e.Logger.Fatal(e.Start(port))
}

// ---------------- HANDLERS ----------------

func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)

	// 1. Binding (Parsear JSON)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Formato de datos inválido"})
	}

	// 2. Validación Estricta (Estructura y reglas de negocio)
	if err := c.Validate(req); err != nil {
		// Retornamos el error de validación específico
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Datos inválidos: " + err.Error()})
	}

	// 3. Validación ReCAPTCHA (Seguridad externa)
	if !validarCaptchaGoogle(req.RecaptchaToken) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Captcha inválido o expirado"})
	}

	// 4. Sanitización básica y creación del modelo
	// (PostgreSQL/GORM manejan SQL Injection, pero limpiamos espacios extra)
	nuevoContacto := ContactoWeb{
		Nombre:    strings.TrimSpace(req.Nombre),
		Email:     strings.TrimSpace(req.Email),
		Telefono:  strings.TrimSpace(req.Telefono),
		Servicio:  strings.TrimSpace(req.Servicio),
		Mensaje:   strings.TrimSpace(req.Mensaje),
		IpUsuario: c.RealIP(),
	}

	// 5. Guardado en BD
	if result := db.Create(&nuevoContacto); result.Error != nil {
		fmt.Println("Error DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error interno al procesar la solicitud"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Enviado correctamente"})
}

// ---------------- HELPERS ----------------

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if token == "" || secret == "" {
		// Loguear error de configuración si falta el secreto
		if secret == "" {
			fmt.Println("❌ Error: RECAPTCHA_SECRET no configurado")
		}
		return false
	}

	client := &http.Client{Timeout: 10 * time.Second} // Timeout para evitar colgar la goroutine
	resp, err := client.PostForm("https://www.google.com/recaptcha/api/siteverify", map[string][]string{
		"secret":   {secret},
		"response": {token},
	})
	if err != nil {
		fmt.Println("Error conectando a Google ReCAPTCHA:", err)
		return false
	}
	defer resp.Body.Close()

	var googleResult RecaptchaResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleResult); err != nil {
		return false
	}
	return googleResult.Success
}
