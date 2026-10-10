package reaction

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type unusedOrigins struct{}

func (unusedOrigins) TaskOrigin(context.Context, string, string) (int64, int64, bool, error) {
	return 0, 0, false, nil
}

type unusedReactor struct{}

func (unusedReactor) SetMessageReaction(context.Context, int64, int64, string) error { return nil }

type unusedTelemetry struct{}

func (unusedTelemetry) RecordReaction(context.Context, string, string) {}

// react is offered only on a watched chat turn that answers the user's own message.
func TestReactIsOfferedOnlyOnWatchedTurnsAnsweringTheUser(t *testing.T) {
	provider, err := NewProvider(unusedOrigins{}, unusedReactor{}, unusedTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	answering := tools.NewRun("thanks")
	answering.MarkUserMessage()
	chat := outbox.ChatTurn("0123456789abcdef")
	cases := []struct {
		name     string
		ctx      context.Context
		readOnly bool
		offered  bool
	}{
		{"chat turn answering the user", tools.WithRun(tools.WithTurn(context.Background(), chat), answering), false, true},
		{"read-only chat turn", tools.WithRun(tools.WithTurn(context.Background(), chat), answering), true, false},
		{"chat turn answering our own message", tools.WithRun(tools.WithTurn(context.Background(), chat), tools.NewRun("continue")), false, false},
		{"scheduled run", tools.WithRun(tools.WithTurn(context.Background(), outbox.ScheduledTurn("0123456789abcdef")), answering), false, false},
	}
	for _, tt := range cases {
		collect := broker.ToolsFor
		if tt.readOnly {
			collect = broker.ReadOnlyToolsFor
		}
		set, err := collect(tt.ctx, "alice")
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if offered := len(set.Definitions) == 1 && set.Definitions[0].Name == "react"; offered != tt.offered || (!tt.offered && set.Guidance != "") {
			t.Errorf("%s: tools = %+v", tt.name, set)
		}
	}
}
