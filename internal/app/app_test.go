package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

type lifecycleModule struct {
	id       string
	startErr error
	started  bool
	stopped  bool
}

func (m *lifecycleModule) Descriptor() module.Descriptor {
	return module.Descriptor{ID: m.id, Version: "1.0.0", Kind: module.KindSystem}
}

func (m *lifecycleModule) Start(context.Context) error {
	m.started = m.startErr == nil
	return m.startErr
}

func (m *lifecycleModule) Ready(context.Context) error { return nil }

func (m *lifecycleModule) Shutdown(context.Context) error {
	m.stopped = true
	return nil
}

func TestFailedStartStopsWhatAlreadyStarted(t *testing.T) {
	first := &lifecycleModule{id: "first"}
	second := &lifecycleModule{id: "second", startErr: errors.New("refused")}
	registry := module.NewRegistry()
	for _, registered := range []*lifecycleModule{first, second} {
		if err := registry.Register(registered); err != nil {
			t.Fatal(err)
		}
	}
	application, err := New(config.Config{
		ListenAddress:   "127.0.0.1:8080",
		ShutdownTimeout: time.Second,
		DataDir:         filepath.Join(t.TempDir(), "data"),
		Timezone:        "UTC",
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	err = application.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `start module "second": refused`) {
		t.Fatalf("Run = %v, want the second module's start error", err)
	}
	if !first.started || !first.stopped {
		t.Fatalf("first module started %v and stopped %v, want both", first.started, first.stopped)
	}
	if second.stopped {
		t.Fatal("the module that failed to start was shut down")
	}
}
