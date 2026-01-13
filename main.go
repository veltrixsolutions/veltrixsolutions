package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gorm.io/driver/postgres" // <--- CAMBIO: Usamos Postgres
	"gorm.io/gorm"
)

// ... (Las estructuras ContactoWeb, ContactoRequest, etc. SE QUEDAN IGUAL) ...
type ContactoWeb struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Nombre    string    `gorm:"type:varchar(100);not null" json:"nombre"` // nvarchar -> varchar
	Email     string    `gorm:"type:varchar(150);not null" json:"email"`
	Telefono  string    `gorm:"type:varchar(20)" json:"telefono"`
	Servicio  string    `gorm:"type:varchar(50)" json:"servicio"`
	Mensaje   string    `gorm:"type:text" json:"mensaje"`
	IpUsuario string    `gorm:"type:varchar(50)" json:"-"`
	CreatedAt time.Time `json:"fecha"`
}

// ... (Resto de structs iguales) ...
type ContactoRequest struct {
	Nombre         string `json:"nombre"`
	Email          string `json:"email"`
	Telefono       string `json:"telefono"`
	Servicio       string `json:"servicio"`
	Mensaje        string `json:"mensaje"`
	RecaptchaToken string `json:"recaptchaToken"`
}

type RecaptchaResponse struct {
	Success bool `json:"success"`
}

var db *gorm.DB

func main() {
	if err := godotenv.Load(); err != nil {
		fmt.Println("ℹ️ Nota: Usando variables de entorno del sistema.")
	}

	// --- CONEXIÓN A RAILWAY (POSTGRES) ---
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("❌ Error: DB_DSN está vacío.")
	}

	var err error
	// CAMBIO AQUÍ: Usamos postgres.Open
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Falló la conexión a Railway: ", err)
	}
	fmt.Println("✅ Conectado exitosamente a PostgreSQL en Railway")

	// Migración
	db.AutoMigrate(&ContactoWeb{})

	// --- SERVIDOR ---
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost},
	}))

	e.Static("/", "public")
	e.POST("/api/contacto", manejarContacto)

	port := os.Getenv("PORT")
	if port == "" {
		port = ":8080"
	}

	e.Logger.Fatal(e.Start(port))
}

// ... (Las funciones manejarContacto y validarCaptchaGoogle SE QUEDAN IGUAL) ...
func manejarContacto(c echo.Context) error {
	req := new(ContactoRequest)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Datos inválidos"})
	}

	if !validarCaptchaGoogle(req.RecaptchaToken) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Captcha inválido"})
	}

	nuevoContacto := ContactoWeb{
		Nombre:    req.Nombre,
		Email:     req.Email,
		Telefono:  req.Telefono,
		Servicio:  req.Servicio,
		Mensaje:   req.Mensaje,
		IpUsuario: c.RealIP(),
	}

	if result := db.Create(&nuevoContacto); result.Error != nil {
		fmt.Println("Error DB:", result.Error)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error al guardar"})
	}

	return c.JSON(http.StatusCreated, map[string]string{"mensaje": "Enviado correctamente"})
}

func validarCaptchaGoogle(token string) bool {
	secret := os.Getenv("RECAPTCHA_SECRET")
	if token == "" || secret == "" {
		return false
	}

	resp, err := http.PostForm("https://www.google.com/recaptcha/api/siteverify", map[string][]string{
		"secret": {secret}, "response": {token},
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
