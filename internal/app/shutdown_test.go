package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownTogetherStopsEveryServiceWithinTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stuck := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	var askedInTime, finished bool
	winding := func(ctx context.Context) error {
		askedInTime = ctx.Err() == nil
		time.Sleep(20 * time.Millisecond)
		finished = ctx.Err() == nil
		return nil
	}

	err := shutdownTogether(ctx, labelled("shutdown stuck", stuck), winding)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the stuck service's deadline", err)
	}
	if !askedInTime || !finished {
		t.Fatalf("the service behind a stuck one was asked in time %v and finished in time %v", askedInTime, finished)
	}
}
