package config

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const minSecretLength = 10

type Config struct {
	DBDir         string
	DBFile        string
	TmpDir        string
	TmpRetention  time.Duration
	PlayerDir     string
	StudioDir     string
	HTTPPort      string
	JWTSign       string
	SecretKey     string
	SecureCookie  bool
	NetEaseRealIP string
	Debug         bool
}

func Load() *Config {
	_ = godotenv.Load() // For development

	return &Config{
		DBDir:         getEnv("AIRSTATION_DB_DIR", filepath.Join("storage")),
		DBFile:        getEnv("AIRSTATION_DB_FILE", "storage.db"),
		TmpDir:        getEnv("AIRSTATION_TMP_DIR", filepath.Join("static", "tmp")),
		TmpRetention:  getEnvDuration("AIRSTATION_TMP_RETENTION", 24*time.Hour),
		PlayerDir:     getEnv("AIRSTATION_PLAYER_DIR", filepath.Join("web", "player", "dist")),
		StudioDir:     getEnv("AIRSTATION_STUDIO_DIR", filepath.Join("web", "studio", "dist")),
		HTTPPort:      getEnv("AIRSTATION_HTTP_PORT", "7331"),
		JWTSign:       getSecret("AIRSTATION_JWT_SIGN"),
		SecretKey:     getSecret("AIRSTATION_SECRET_KEY"),
		SecureCookie:  getEnvBool("AIRSTATION_SECURE_COOKIE", false),
		NetEaseRealIP: getEnv("AIRSTATION_NETEASE_REAL_IP", ""),
		Debug:         getEnvBool("AIRSTATION_DEBUG", false),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}

	val = strings.ToLower(val)
	return val == "1" || val == "true" || val == "yes" || val == "on"
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Fatal(key + " must be a valid duration, for example 24h")
	}
	if duration <= 0 {
		log.Fatal(key + " must be greater than 0")
	}

	return duration
}

func getSecret(key string) string {
	secretKey := os.Getenv(key)

	if secretKey == "" {
		log.Fatal(key + " environment variable is not set")
	}

	if len(secretKey) < minSecretLength {
		log.Fatal(key + " is too short")
	}

	return secretKey
}
