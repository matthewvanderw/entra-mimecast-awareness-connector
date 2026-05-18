package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/config"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/connector"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/entra"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/mimecast"
	"github.com/robfig/cron/v3"
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

	c := cron.New()
	if _, err := c.AddFunc(cfg.CronExpression, func() {
		if err := conn.Run(context.Background()); err != nil {
			slog.Error("compliance run failed", "error", err)
		}
	}); err != nil {
		slog.Error("invalid cron expression", "expression", cfg.CronExpression, "error", err)
		os.Exit(1)
	}

	c.Start()
	slog.Info("connector scheduled", "expression", cfg.CronExpression, "dry_run", cfg.DryRun)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutdown signal received — waiting for in-flight run to complete")
	stopCtx := c.Stop()
	<-stopCtx.Done()
	slog.Info("connector stopped")
}
