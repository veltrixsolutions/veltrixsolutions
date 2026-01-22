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
// 1. MODELOS DE DATOS (STRUCTS)
// ---------------------------------------------------------

// Modelo para la tabla de imagenes usuarios(GORM)
type ImagenUsuario struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Nombre    string    `gorm:"type:varchar(255)" json:"nombre"`
	Datos     []byte    `gorm:"type:bytea" json:"-"`              // Aquí se guarda la imagen binaria
	MimeType  string    `gorm:"type:varchar(50)" json:"mimeType"` // ej: image/jpeg
	CreatedAt time.Time `json:"fecha"`
}

// Modelo para la Base de Datos (GORM)
type ContactoWeb struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Nombre    string    `gorm:"type:varchar(100);not null" json:"nombre"`
	Email     string    `gorm:"type:varchar(150);not null" json:"email"`
	Telefono  string    `gorm:"type:varchar(20)" json:"telefono"`
	Servicio  string    `gorm:"type:varchar(50)" json:"servicio"`
	Mensaje   string    `gorm:"type:text" json:"mensaje"`
	IpUsuario string    `gorm:"type:varchar(50)" json:"-"` // No se envía al frontend
	CreatedAt time.Time `json:"fecha"`
}

// Modelo para la Petición JSON (Con Validaciones)
type ContactoRequest struct {
	Nombre         string `json:"nombre" validate:"required,min=2,max=100"`
	Email          string `json:"email" validate:"required,email,max=150"`
	Telefono       string `json:"telefono" validate:"omitempty,numeric,max=20"` // Solo números
	Servicio       string `json:"servicio" validate:"max=50"`
	Mensaje        string `json:"mensaje" validate:"required,min=10,max=2000"` // Evita spam vacío
	RecaptchaToken string `json:"recaptchaToken" validate:"required"`
}

// Respuesta de Google ReCAPTCHA
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
		// Devuelve error 400 si la validación falla
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return nil
}

// Variable global para la DB
var db *gorm.DB

// ---------------------------------------------------------
// 3. MANEJO DE ERRORES PERSONALIZADO (404 HTML vs JSON)
// ---------------------------------------------------------

func customHTTPErrorHandler(err error, c echo.Context) {
	code := http.StatusInternalServerError
	if he, ok := err.(*echo.HTTPError); ok {
		code = he.Code
	}

	// Lógica especial para error 404 (Not Found)
	if code == http.StatusNotFound {
		// IMPORTANTE: Si la ruta NO empieza con /api/, servimos el HTML visual
		if !strings.HasPrefix(c.Path(), "/api/") {
			if err := c.File("public/404.html"); err != nil {
				c.Logger().Error(err)
				c.String(code, "404 - Página no encontrada")
			}
			return
		}
	}

	// Para errores de API (JSON) o errores 500, usamos el default de Echo
	c.Echo().DefaultHTTPErrorHandler(err, c)
}

// ---------------------------------------------------------
// 4. FUNCIÓN MAIN (Punto de Entrada)
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

	// Migración automática (Crea la tabla si no existe)
	db.AutoMigrate(&ContactoWeb{}, &ImagenUsuario{})

	// C. Inicializar Echo
	e := echo.New()

	// Asignar Validador y Manejador de Errores
	e.Validator = &CustomValidator{validator: validator.New()}
	e.HTTPErrorHandler = customHTTPErrorHandler

	// ---------------------------------------------------------
	// 5. MIDDLEWARES (Capas de Seguridad)
	// ---------------------------------------------------------

	e.Use(middleware.Logger())  // Logs de peticiones
	e.Use(middleware.Recover()) // Evita que el server se caiga por un panic
	e.Use(middleware.Secure())  // Headers de seguridad (XSS, HSTS)

	// Limita el cuerpo de la petición a 2KB (Suficiente para texto, evita ataques DoS)
	e.Use(middleware.BodyLimit("2M"))

	// Configuración CORS (Cross-Origin Resource Sharing)
	allowOrigin := os.Getenv("FRONTEND_URL")
	if allowOrigin == "" {
		fmt.Println("⚠️ ADVERTENCIA: CORS abierto (*). Configura FRONTEND_URL en producción.")
		allowOrigin = "*"
	}
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{allowOrigin},
		AllowMethods: []string{http.MethodPost, http.MethodGet},
	}))

	// Rate Limiter: 5 peticiones/segundo por IP (Protección anti-spam/DoS)
	configRateLimit := middleware.RateLimiterConfig{
		Skipper: middleware.DefaultSkipper,
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(
			middleware.RateLimiterMemoryStoreConfig{Rate: rate.Limit(5), Burst: 10, ExpiresIn: 3 * time.Minute},
		),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			return ctx.RealIP(), nil
		},
		ErrorHandler: func(context echo.Context, err error) error {
			return context.JSON(http.StatusTooManyRequests, map[string]string{"error": "Demasiadas peticiones. Calma un poco."})
		},
		DenyHandler: func(context echo.Context, identifier string, err error) error {
			return context.JSON(http.StatusTooManyRequests, map[string]string{"error": "Demasiadas peticiones."})
		},
	}
	e.Use(middleware.RateLimiterWithConfig(configRateLimit))

	// ---------------------------------------------------------
	// 6. RUTAS
	// ---------------------------------------------------------

	// Servir archivos estáticos (Frontend: HTML, CSS, JS, Imágenes)
	// Esto servirá index.html en la raíz "/"
	e.Static("/", "public")

	// Ruta API para el formulario
	e.POST("/api/contacto", manejarContacto)

	// Ruta API para las imagenes
	e.POST("/api/subir-imagen", subirImagen)

	// ---------------------------------------------------------
	// 7. ARRANQUE DEL SERVIDOR
	// ---------------------------------------------------------

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// Asegurar formato ":8080"
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	fmt.Println("🚀 Servidor Veltrix corriendo en el puerto " + port)
	e.Logger.Fatal(e.Start(port))
}

// ---------------------------------------------------------
// 8. HANDLERS (Lógica de Negocio)
// ---------------------------------------------------------

func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)

	// 1. Parsear JSON
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Formato JSON inválido"})
	}

	// 2. Validar Datos (Reglas del struct)
	if err := c.Validate(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Datos inválidos: " + err.Error()})
	}

	// 3. Verificar ReCAPTCHA con Google
	if !validarCaptchaGoogle(req.RecaptchaToken) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Verificación de seguridad fallida (Captcha)"})
	}

	// 4. Crear objeto para guardar (Sanitización básica)
	nuevoContacto := ContactoWeb{
		Nombre:    strings.TrimSpace(req.Nombre),
		Email:     strings.TrimSpace(req.Email),
		Telefono:  strings.TrimSpace(req.Telefono),
		Servicio:  strings.TrimSpace(req.Servicio),
		Mensaje:   strings.TrimSpace(req.Mensaje),
		IpUsuario: c.RealIP(),
	}

	// 5. Guardar en Base de Datos
	if result := db.Create(&nuevoContacto); result.Error != nil {
		fmt.Println("❌ Error guardando en DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error interno del servidor"})
	}

	// 6. Responder éxito
	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Solicitud recibida correctamente"})
}

// ---------------------------------------------------------
// 9. HELPERS
// ---------------------------------------------------------

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if secret == "" {
		fmt.Println("❌ Error: RECAPTCHA_SECRET no está configurado en el .env")
		return false // Falla segura si no hay configuración
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm("https://www.google.com/recaptcha/api/siteverify", map[string][]string{
		"secret":   {secret},
		"response": {token},
	})
	if err != nil {
		fmt.Println("Error conectando a Google:", err)
		return false
	}
	defer resp.Body.Close()

	var googleResult RecaptchaResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleResult); err != nil {
		return false
	}
	return googleResult.Success
}

// Pon esto al final, junto con tus otros handlers
func subirImagen(c echo.Context) error {
	// 1. Obtener el archivo del formulario (key: "imagen")
	file, err := c.FormFile("imagen")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No se ha enviado ninguna imagen"})
	}

	// 2. Validar tamaño (Ejemplo: Máximo 2MB para no saturar la BD)
	if file.Size > 2*1024*1024 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "La imagen es muy pesada (Máximo 2MB)"})
	}

	// 3. Abrir el archivo
	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al procesar el archivo"})
	}
	defer src.Close()

	// 4. Leer los bytes (Binario)
	fileBytes, err := io.ReadAll(src)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al leer el archivo"})
	}

	// 5. Crear registro para la BD
	nuevaImagen := ImagenUsuario{
		Nombre:   file.Filename,
		Datos:    fileBytes, // Guardamos el binario
		MimeType: file.Header.Get("Content-Type"),
	}

	// 6. Guardar en Postgres
	if result := db.Create(&nuevaImagen); result.Error != nil {
		fmt.Println("Error DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al guardar en la base de datos"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Imagen subida exitosamente"})
}
