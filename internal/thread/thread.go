// Package thread carries the Telegram tab a piece of work belongs to. A send made under a context
// with a thread lands in that tab; one made without lands in the chat's main view, which is where
// anything nobody asked for in a tab belongs.
package thread

import "context"

type contextKey struct{}

// With returns a context whose sends go to the given tab; zero means the main view.
func With(ctx context.Context, threadID int64) context.Context {
	if threadID <= 0 {
		return context.WithValue(ctx, contextKey{}, int64(0))
	}
	return context.WithValue(ctx, contextKey{}, threadID)
}

// From returns the tab of the context, or zero for the main view.
func From(ctx context.Context) int64 {
	threadID, _ := ctx.Value(contextKey{}).(int64)
	return threadID
}
