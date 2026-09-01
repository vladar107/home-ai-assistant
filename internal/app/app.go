package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/health"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

type App struct {
	cfg     config.Config
	logger  *slog.Logger
	modules []module.Module
	server  *http.Server
	ready   atomic.Bool
}

func New(cfg config.Config, registry *module.Registry, logger *slog.Logger) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, fmt.Errorf("module registry is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}

	modules, err := registry.Ordered()
	if err != nil {
		return nil, err
	}

	application := &App{
		cfg:     cfg,
		logger:  logger,
		modules: modules,
	}
	application.server = &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           health.Handler(application),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return application, nil
}

func (a *App) Run(ctx context.Context) error {
	started, err := a.startModules(ctx)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", a.cfg.ListenAddress)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		shutdownErr := a.shutdownModules(shutdownCtx, started)
		cancel()
		return errors.Join(fmt.Errorf("listen on %s: %w", a.cfg.ListenAddress, err), shutdownErr)
	}

	a.ready.Store(true)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- a.server.Serve(listener)
	}()

	a.logger.Info("application started", "address", listener.Addr().String(), "modules", len(a.modules))

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve health endpoint: %w", err)
		}
	}

	a.ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutdown HTTP server: %w", err))
	}
	if err := a.shutdownModules(shutdownCtx, started); err != nil {
		runErr = errors.Join(runErr, err)
	}

	a.logger.Info("application stopped")
	return runErr
}

func (a *App) Ready(ctx context.Context) error {
	if !a.ready.Load() {
		return fmt.Errorf("application is not ready")
	}
	for _, registeredModule := range a.modules {
		if err := registeredModule.Ready(ctx); err != nil {
			return fmt.Errorf("module %q: %w", registeredModule.Descriptor().ID, err)
		}
	}
	return nil
}

func (a *App) startModules(ctx context.Context) ([]module.Module, error) {
	started := make([]module.Module, 0, len(a.modules))
	for _, registeredModule := range a.modules {
		if err := registeredModule.Start(ctx); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
			shutdownErr := a.shutdownModules(shutdownCtx, started)
			cancel()
			return nil, errors.Join(
				fmt.Errorf("start module %q: %w", registeredModule.Descriptor().ID, err),
				shutdownErr,
			)
		}
		started = append(started, registeredModule)
	}
	return started, nil
}

func (a *App) shutdownModules(ctx context.Context, started []module.Module) error {
	var shutdownErr error
	for i := len(started) - 1; i >= 0; i-- {
		registeredModule := started[i]
		if err := registeredModule.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(
				shutdownErr,
				fmt.Errorf("shutdown module %q: %w", registeredModule.Descriptor().ID, err),
			)
		}
	}
	return shutdownErr
}
