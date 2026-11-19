package tools

import (
	"context"
	"slices"
)

// Scope confines the tools of one run: it is offered only the named tools, and Bind, when set,
// adds to the context of each of its calls what a provider needs to narrow them further, such as
// the one mail thread the run may read. Writes names those of the tools a read-only run is offered
// although they write, each a narrow exception its provider bounds itself.
type Scope struct {
	Tools  []string
	Writes []string
	Bind   func(context.Context) context.Context
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

// writes reports whether a scope lets a read-only run be offered the write tool; no scope lets none.
func (s *Scope) writes(name string) bool {
	return s != nil && slices.Contains(s.Writes, name) && slices.Contains(s.Tools, name)
}

// bind adds the scope's binding to ctx; without a scope or a binding, ctx is unchanged.
func (s *Scope) bind(ctx context.Context) context.Context {
	if s == nil || s.Bind == nil {
		return ctx
	}
	return s.Bind(ctx)
}
