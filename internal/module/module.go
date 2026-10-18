package module

import "context"

type Kind string

const (
	KindSystem    Kind = "system"
	KindConnector Kind = "connector"
)

type Descriptor struct {
	ID      string
	Version string
	Kind    Kind
}

type Module interface {
	Descriptor() Descriptor
	Start(context.Context) error
	Ready(context.Context) error
	Shutdown(context.Context) error
}
