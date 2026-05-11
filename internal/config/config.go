package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	CronExpression string

	EntraTenantID     string
	EntraClientID     string
	EntraClientSecret string

	// Mimecast 2.0 API credentials — obtained from the Mimecast Administration
	// Console under Services > API and Platform Integrations.
	MimecastBaseURL      string
	MimecastClientID     string
	MimecastClientSecret string

	// PageSize controls records per paginated call for both Entra and Mimecast.
	PageSize int

	// DisableThreshold is the minimum number of unwatched training videos that
	// triggers an account disable. Defaults to 4.
	DisableThreshold int

	// DryRun logs which accounts would be disabled without making any changes.
	DryRun bool
}

func Load() (*Config, error) {
	cfg := &Config{
		CronExpression:    getEnv("CRON_EXPRESSION", "0 0 * * *"),
		EntraTenantID:     getEnv("ENTRA_TENANT_ID", ""),
		EntraClientID:     getEnv("ENTRA_CLIENT_ID", ""),
		EntraClientSecret: getEnv("ENTRA_CLIENT_SECRET", ""),
		MimecastBaseURL:      getEnv("MIMECAST_BASE_URL", "https://api.services.mimecast.com"),
		MimecastClientID:     getEnv("MIMECAST_CLIENT_ID", ""),
		MimecastClientSecret: getEnv("MIMECAST_CLIENT_SECRET", ""),
		PageSize:         50,
		DisableThreshold: 4,
		DryRun:           os.Getenv("DRY_RUN") == "true" || os.Getenv("DRY_RUN") == "1",
	}

	if raw := os.Getenv("PAGE_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("PAGE_SIZE must be a positive integer, got %q", raw)
		}
		cfg.PageSize = n
	}

	if raw := os.Getenv("DISABLE_THRESHOLD"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("DISABLE_THRESHOLD must be a positive integer, got %q", raw)
		}
		cfg.DisableThreshold = n
	}

	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	required := []struct{ name, val string }{
		{"ENTRA_TENANT_ID", c.EntraTenantID},
		{"ENTRA_CLIENT_ID", c.EntraClientID},
		{"ENTRA_CLIENT_SECRET", c.EntraClientSecret},
		{"MIMECAST_CLIENT_ID", c.MimecastClientID},
		{"MIMECAST_CLIENT_SECRET", c.MimecastClientSecret},
	}
	for _, r := range required {
		if r.val == "" {
			return fmt.Errorf("required environment variable %s is not set", r.name)
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
