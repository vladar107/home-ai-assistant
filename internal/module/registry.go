package module

import (
	"fmt"
	"regexp"
	"slices"
)

var (
	moduleIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	moduleVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

type Registry struct {
	ids     map[string]struct{}
	modules []Module
}

func NewRegistry() *Registry {
	return &Registry{ids: make(map[string]struct{})}
}

func (r *Registry) Register(m Module) error {
	if m == nil {
		return fmt.Errorf("register module: module is nil")
	}

	descriptor := m.Descriptor()
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	if _, exists := r.ids[descriptor.ID]; exists {
		return fmt.Errorf("register module %q: duplicate ID", descriptor.ID)
	}

	r.ids[descriptor.ID] = struct{}{}
	r.modules = append(r.modules, m)
	return nil
}

// Ordered returns the modules in registration order, the order the application starts them in.
func (r *Registry) Ordered() []Module {
	return slices.Clone(r.modules)
}

func validateDescriptor(descriptor Descriptor) error {
	if !moduleIDPattern.MatchString(descriptor.ID) {
		return fmt.Errorf("register module: invalid ID %q", descriptor.ID)
	}
	if !moduleVersionPattern.MatchString(descriptor.Version) {
		return fmt.Errorf("register module %q: invalid version %q", descriptor.ID, descriptor.Version)
	}
	if !slices.Contains([]Kind{KindSystem, KindConnector}, descriptor.Kind) {
		return fmt.Errorf("register module %q: invalid kind %q", descriptor.ID, descriptor.Kind)
	}
	return nil
}
