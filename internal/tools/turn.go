package tools

import "context"

type turnKey struct{}

// WithTurn names the turn a context belongs to, so a tool can leave something for that turn's
// reply, such as a card to send with it.
func WithTurn(ctx context.Context, turn string) context.Context {
	return context.WithValue(ctx, turnKey{}, turn)
}

// TurnFrom returns the turn a tool call belongs to; an empty key means the call has no turn.
func TurnFrom(ctx context.Context) string {
	turn, _ := ctx.Value(turnKey{}).(string)
	return turn
}
