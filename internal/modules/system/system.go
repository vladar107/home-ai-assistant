package system

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/vladar107/home-ai-assistant/internal/module"
)

type Module struct {
	running atomic.Bool
}

func New() *Module {
	return &Module{}
}

func (m *Module) Descriptor() module.Descriptor {
	return module.Descriptor{
		ID:      "system",
		Version: "0.1.0",
		Kind:    module.KindSystem,
	}
}

func (m *Module) Start(context.Context) error {
	m.running.Store(true)
	return nil
}

func (m *Module) Ready(context.Context) error {
	if !m.running.Load() {
		return fmt.Errorf("system module is not running")
	}
	return nil
}

func (m *Module) Shutdown(context.Context) error {
	m.running.Store(false)
	return nil
}
