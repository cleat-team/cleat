package backendkit

import "os"

// Config holds the core application configuration shared by all backends.
// Backends with additional fields should embed this struct.
type Config struct {
	CleatURL   string
	Port       string
	APIKey     string // CLEAT_API_KEY — when set, enables auth middleware
	CORSOrigin string // CLEAT_CORS_ORIGIN — scoped CORS origin (default "*")
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		CleatURL:   getEnv("CLEAT_URL", "http://localhost:8080"),
		Port:       getEnv("PORT", "9090"),
		APIKey:     os.Getenv("CLEAT_API_KEY"),
		CORSOrigin: getEnv("CLEAT_CORS_ORIGIN", "*"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
