package tools

import (
	"context"
	"slices"
)

// Scope confines the tools of one run: it is offered only the named tools, and Bind, when set,
// adds to the context of each of its calls what a provider needs to narrow them further, such as
// the one mail thread the run may read.
type Scope struct {
	Tools []string
	Bind  func(context.Context) context.Context
}

type scopeKey struct{}

// WithScope confines the tool set collected under ctx to the scope.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope)
}

func scopeFrom(ctx context.Context) *Scope {
	scope, _ := ctx.Value(scopeKey{}).(*Scope)
	return scope
}

// offers reports whether a scope, nil for none, lets the run be offered the tool.
func (s *Scope) offers(name string) bool {
	return s == nil || slices.Contains(s.Tools, name)
}

// bind adds the scope's binding to ctx; without a scope or a binding, ctx is unchanged.
func (s *Scope) bind(ctx context.Context) context.Context {
	if s == nil || s.Bind == nil {
		return ctx
	}
	return s.Bind(ctx)
}
