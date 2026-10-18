package tools

import "context"

type budgetKey struct{}

// WithCallBudget sets how many calls the tool set collected under ctx may make, in place of the
// per-turn default; a background task's run gets a larger budget, and zero refuses every call.
func WithCallBudget(ctx context.Context, calls int) context.Context {
	return context.WithValue(ctx, budgetKey{}, max(calls, 0))
}

func callBudgetFrom(ctx context.Context) int32 {
	if calls, set := ctx.Value(budgetKey{}).(int); set {
		return int32(calls)
	}
	return maximumCallsPerTurn
}
