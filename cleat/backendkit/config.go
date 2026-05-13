package backendkit

import "os"

// Config holds the core application configuration shared by all backends.
// Backends with additional fields should embed this struct.
type Config struct {
	CleatURL string
	Port     string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		CleatURL: getEnv("CLEAT_URL", "http://localhost:8080"),
		Port:     getEnv("PORT", "9090"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
