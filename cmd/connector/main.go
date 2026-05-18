package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/config"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/connector"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/entra"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/mimecast"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	entraClient := entra.NewClient(cfg.EntraTenantID, cfg.EntraClientID, cfg.EntraClientSecret)
	mimecastClient := mimecast.NewClient(
		cfg.MimecastBaseURL,
		cfg.MimecastClientID,
		cfg.MimecastClientSecret,
	)
	conn := connector.New(entraClient, mimecastClient, cfg.PageSize, cfg.DisableThreshold, cfg.DryRun)

	slog.Info("starting compliance run", "dry_run", cfg.DryRun)
	if err := conn.Run(context.Background()); err != nil {
		slog.Error("compliance run failed", "error", err)
		os.Exit(1)
	}
	slog.Info("compliance run complete")
}
