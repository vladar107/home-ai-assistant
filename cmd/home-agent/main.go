package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vladar107/home-ai-assistant/internal/app"
	"github.com/vladar107/home-ai-assistant/internal/config"
	modulepkg "github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/modules/system"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	registry := modulepkg.NewRegistry()
	if err := registry.Register(system.New()); err != nil {
		logger.Error("register module", "error", err)
		os.Exit(1)
	}

	application, err := app.New(cfg, registry, logger)
	if err != nil {
		logger.Error("initialize application", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := application.Run(ctx); err != nil {
		logger.Error("application stopped", "error", err)
		os.Exit(1)
	}
}
