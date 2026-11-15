package tools

import (
	"context"
	"testing"
)

func TestGuidanceOverrideReplacesTheOfferingProvidersGuidanceOnly(t *testing.T) {
	broker := testBroker(t)
	first := &fakeProvider{principal: testPrincipal, definitions: []Definition{testDefinition("first_tool", 0)}}
	if err := broker.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(&otherProvider{}); err != nil {
		t.Fatal(err)
	}

	set, err := broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if want := "test guidance\nother guidance\nother write guidance"; set.Guidance != want {
		t.Fatalf("guidance = %q, want %q", set.Guidance, want)
	}

	broker.SetGuidanceOverrides(map[string]string{"other_tool": "replaced guidance"})
	set, err = broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if want := "test guidance\nreplaced guidance"; set.Guidance != want {
		t.Fatalf("guidance = %q, want %q", set.Guidance, want)
	}
	if len(set.Definitions) != 2 {
		t.Fatalf("definitions = %d, want both tools still offered", len(set.Definitions))
	}
}

type otherProvider struct{}

func (otherProvider) Contribute(_ context.Context, principal string) (Contribution, error) {
	if principal != testPrincipal {
		return Contribution{}, nil
	}
	return Contribution{
		Guidance: "other guidance", WriteGuidance: "other write guidance",
		Definitions: []Definition{testDefinition("other_tool", 0)},
	}, nil
}

func (otherProvider) Execute(context.Context, string, Call) (Result, error) { return Result{}, nil }
