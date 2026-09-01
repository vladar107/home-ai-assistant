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

type registration struct {
	module     Module
	descriptor Descriptor
}

type Registry struct {
	modules map[string]registration
	order   []string
}

func NewRegistry() *Registry {
	return &Registry{modules: make(map[string]registration)}
}

func (r *Registry) Register(m Module) error {
	if m == nil {
		return fmt.Errorf("register module: module is nil")
	}

	descriptor := m.Descriptor()
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	if _, exists := r.modules[descriptor.ID]; exists {
		return fmt.Errorf("register module %q: duplicate ID", descriptor.ID)
	}

	descriptor.Dependencies = slices.Clone(descriptor.Dependencies)
	r.modules[descriptor.ID] = registration{module: m, descriptor: descriptor}
	r.order = append(r.order, descriptor.ID)
	return nil
}

func (r *Registry) Ordered() ([]Module, error) {
	if err := r.validateDependencies(); err != nil {
		return nil, err
	}

	const (
		unvisited = iota
		visiting
		visited
	)
	states := make(map[string]int, len(r.modules))
	ordered := make([]Module, 0, len(r.modules))

	var visit func(string) error
	visit = func(name string) error {
		switch states[name] {
		case visiting:
			return fmt.Errorf("module dependency cycle includes %q", name)
		case visited:
			return nil
		}

		states[name] = visiting
		for _, dependency := range r.modules[name].descriptor.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[name] = visited
		ordered = append(ordered, r.modules[name].module)
		return nil
	}

	for _, name := range r.order {
		if err := visit(name); err != nil {
			return nil, err
		}
	}

	return ordered, nil
}

func (r *Registry) validateDependencies() error {
	for _, name := range r.order {
		for _, dependency := range r.modules[name].descriptor.Dependencies {
			if _, exists := r.modules[dependency]; !exists {
				return fmt.Errorf("module %q depends on unregistered module %q", name, dependency)
			}
		}
	}
	return nil
}

func validateDescriptor(descriptor Descriptor) error {
	if !moduleIDPattern.MatchString(descriptor.ID) {
		return fmt.Errorf("register module: invalid ID %q", descriptor.ID)
	}
	if !moduleVersionPattern.MatchString(descriptor.Version) {
		return fmt.Errorf("register module %q: invalid version %q", descriptor.ID, descriptor.Version)
	}
	if !slices.Contains([]Kind{KindSystem, KindConnector, KindAutomation, KindEdgeWorker}, descriptor.Kind) {
		return fmt.Errorf("register module %q: invalid kind %q", descriptor.ID, descriptor.Kind)
	}

	dependencies := make(map[string]struct{}, len(descriptor.Dependencies))
	for _, dependency := range descriptor.Dependencies {
		if !moduleIDPattern.MatchString(dependency) {
			return fmt.Errorf("register module %q: invalid dependency ID %q", descriptor.ID, dependency)
		}
		if dependency == descriptor.ID {
			return fmt.Errorf("register module %q: cannot depend on itself", descriptor.ID)
		}
		if _, exists := dependencies[dependency]; exists {
			return fmt.Errorf("register module %q: duplicate dependency %q", descriptor.ID, dependency)
		}
		dependencies[dependency] = struct{}{}
	}

	return nil
}
