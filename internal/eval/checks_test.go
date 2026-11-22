package eval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int       { return &n }
func strp(s string) *string { return &s }

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func toolCall(name, arguments string) ToolCall {
	return ToolCall{Name: name, Arguments: json.RawMessage(arguments)}
}

func TestChecksOnHandMadeRecords(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	reminder := toolCall("schedule_create", `{"name":"Dentist","when":{"at":"2026-03-10T15:00"},"events":[{"id":"a"},{"id":"b"}],"count":2}`)
	tests := []struct {
		name   string
		expect Expect
		record TurnRecord
		want   []string // substrings of the failures' checks and messages, in order; none means a pass
	}{
		{name: "nil-free empty expectation passes", expect: Expect{}, record: TurnRecord{}},
		{
			name:   "tool called with matching args",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "name", Equals: raw(t, "Dentist")}, {Path: "count", Equals: raw(t, 2)}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "tool never called",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1)}}},
			record: TurnRecord{},
			want:   []string{"calls", "schedule_create", "never called"},
		},
		{
			name:   "args mismatch names the path and a capped value",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "name", Contains: strp("Barber")}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
			want:   []string{"calls", "args.name", "Barber"},
		},
		{
			name:   "nested path with an index",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "events.1.id", Equals: raw(t, "b")}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "missing path",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "events.5.id", Contains: strp("a")}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
			want:   []string{"calls", "args.events.5.id missing"},
		},
		{
			name:   "any_of accepts a call of either tool",
			expect: Expect{Calls: []Call{{AnyOf: []string{"schedule_update", "schedule_create"}, Min: intp(1), Args: []ArgExpr{{Path: "name", Equals: raw(t, "Dentist")}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "any_of fails when none of its tools was called",
			expect: Expect{Calls: []Call{{AnyOf: []string{"schedule_update", "schedule_delete"}, Min: intp(1)}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
			want:   []string{"calls", "schedule_update or schedule_delete", "never called"},
		},
		{
			name:   "matches a pattern",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "name", Matches: strp(`^Den`)}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "within a date-time passes",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "when.at", Within: &Within{From: "2026-03-10T14:55", To: "2026-03-10T15:05"}}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "within a date-time fails outside",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "when.at", Within: &Within{From: "2026-03-11T00:00", To: "2026-03-11T23:59"}}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
			want:   []string{"calls", "outside 2026-03-11T00:00"},
		},
		{
			name:   "within a time of day",
			expect: Expect{Calls: []Call{{Tool: "schedule_create", Min: intp(1), Args: []ArgExpr{{Path: "when.at", Within: &Within{From: "14:00", To: "16:00"}}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{reminder}},
		},
		{
			name:   "offset time is read in the case zone",
			expect: Expect{Calls: []Call{{Tool: "t", Min: intp(1), Args: []ArgExpr{{Path: "at", Within: &Within{From: "15:00", To: "15:00"}}}}}},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("t", `{"at":"2026-03-10T14:00:00Z"}`)}},
		},
		{
			name:   "call count above max",
			expect: Expect{Calls: []Call{{Tool: "t", Min: intp(1), Max: intp(1)}}},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("t", `{}`), toolCall("t", `{}`)}},
			want:   []string{"calls", "at most 1", "got 2"},
		},
		{
			name:   "not called",
			expect: Expect{NotCalled: []string{"gmail_send"}},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("gmail_send", `{}`)}},
			want:   []string{"not_called", "gmail_send"},
		},
		{
			name:   "max calls",
			expect: Expect{MaxCalls: intp(1)},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("a", `{}`), toolCall("b", `{}`)}},
			want:   []string{"max_calls", "2 tool calls"},
		},
		{
			name:   "ordered holds",
			expect: Expect{Ordered: true, Calls: []Call{{Tool: "a"}, {Tool: "b"}}},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("a", `{}`), toolCall("b", `{}`)}},
		},
		{
			name:   "ordered broken",
			expect: Expect{Ordered: true, Calls: []Call{{Tool: "a", Min: intp(1)}, {Tool: "b", Min: intp(1)}}},
			record: TurnRecord{ToolCalls: []ToolCall{toolCall("b", `{}`), toolCall("a", `{}`)}},
			want:   []string{"calls", "b", "before the previous"},
		},
		{
			name:   "proposal count and tools",
			expect: Expect{Proposals: &Proposals{Min: 1, Max: intp(1), Tools: []string{"schedule_create"}}},
			record: TurnRecord{Proposals: []Proposal{{Tool: "schedule_create"}}},
		},
		{
			name:   "proposal missing tool",
			expect: Expect{Proposals: &Proposals{Min: 1, Tools: []string{"memory_add"}}},
			record: TurnRecord{Proposals: []Proposal{{Tool: "schedule_create"}}},
			want:   []string{"proposals", "no proposal of memory_add"},
		},
		{
			name:   "too many proposals",
			expect: Expect{Proposals: &Proposals{Max: intp(0)}},
			record: TurnRecord{Proposals: []Proposal{{Tool: "schedule_create"}}},
			want:   []string{"proposals", "at most 0"},
		},
		{
			name:   "no writes with a proposal",
			expect: Expect{NoWrites: true},
			record: TurnRecord{Proposals: []Proposal{{Tool: "schedule_create"}}},
			want:   []string{"no_writes", "1 proposal"},
		},
		{
			name:   "no writes after a rejection",
			expect: Expect{NoWrites: true},
			record: TurnRecord{Resolved: []Proposal{{Tool: "schedule_create", Status: "rejected"}}},
		},
		{
			name:   "no writes after a confirmation",
			expect: Expect{NoWrites: true},
			record: TurnRecord{Resolved: []Proposal{{Tool: "schedule_create", Status: "executed"}}},
			want:   []string{"no_writes", "schedule_create was confirmed"},
		},
		{
			name:   "reply contains is case-insensitive",
			expect: Expect{Reply: &ReplyExpect{Contains: []string{"TUESDAY"}, NotContains: []string{"monday"}}},
			record: TurnRecord{Reply: "It is Tuesday."},
		},
		{
			name:   "reply lacks and contains",
			expect: Expect{Reply: &ReplyExpect{Contains: []string{"wednesday"}, NotContains: []string{"tuesday"}}},
			record: TurnRecord{Reply: "It is Tuesday."},
			want:   []string{"reply.contains", "wednesday", "reply.not_contains", "tuesday"},
		},
		{
			name:   "reply matches",
			expect: Expect{Reply: &ReplyExpect{Matches: []string{`\d{2}:\d{2}`}}},
			record: TurnRecord{Reply: "At 15:00"},
		},
		{
			name:   "reply pattern misses",
			expect: Expect{Reply: &ReplyExpect{Matches: []string{`^\d+$`}}},
			record: TurnRecord{Reply: "At 15:00"},
			want:   []string{"reply.matches"},
		},
		{
			name:   "reply not_matches passes when the pattern is absent",
			expect: Expect{Reply: &ReplyExpect{NotMatches: []string{`(?i)tomorrow|today`}}},
			record: TurnRecord{Reply: "At 15:00"},
		},
		{
			name:   "reply not_matches fails when the pattern is present",
			expect: Expect{Reply: &ReplyExpect{NotMatches: []string{`(?i)tomorrow|today`}}},
			record: TurnRecord{Reply: "Today at 15:00"},
			want:   []string{"reply.not_matches"},
		},
		{
			name:   "language russian passes",
			expect: Expect{Reply: &ReplyExpect{Language: "ru"}},
			record: TurnRecord{Reply: "Сегодня вторник, 10 марта (Berlin)."},
		},
		{
			name:   "language russian fails on english",
			expect: Expect{Reply: &ReplyExpect{Language: "ru"}},
			record: TurnRecord{Reply: "Today is Tuesday."},
			want:   []string{"reply.language", "0% Cyrillic"},
		},
		{
			name:   "language english fails on russian",
			expect: Expect{Reply: &ReplyExpect{Language: "en"}},
			record: TurnRecord{Reply: "Сегодня вторник"},
			want:   []string{"reply.language", "100% Cyrillic"},
		},
		{
			name:   "quiet run",
			expect: Expect{Reply: &ReplyExpect{Quiet: true}},
			record: TurnRecord{Quiet: true},
		},
		{
			name:   "quiet expected but a reply came",
			expect: Expect{Reply: &ReplyExpect{Quiet: true}},
			record: TurnRecord{Reply: "Here is the news"},
			want:   []string{"reply.quiet", "Here is the news"},
		},
		{
			name:   "failure excerpt is capped",
			expect: Expect{Reply: &ReplyExpect{Contains: []string{"absent"}}},
			record: TurnRecord{Reply: strings.Repeat("long reply ", 100)},
			want:   []string{"reply.contains", "…"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			failures := CheckStep(t.Context(), &test.expect, test.record, CheckEnv{Location: berlin})
			if len(test.want) == 0 {
				if len(failures) != 0 {
					t.Fatalf("unexpected failures: %+v", failures)
				}
				return
			}
			var text []string
			for _, failure := range failures {
				text = append(text, failure.Check+": "+failure.Message)
				if len(failure.Message) > 400 {
					t.Fatalf("failure message is not capped: %d bytes", len(failure.Message))
				}
			}
			joined := strings.Join(text, "\n")
			for _, want := range test.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("failures lack %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestStoreChecksReadTheWorldsStore(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionJobs: []jobSeed{
			{ID: "morning-news", Name: "Morning news", Prompt: "Summarise the news.", Cron: "0 9 * * *"},
			{ID: "call-dentist", Reminder: "Call the dentist", At: "2026-03-10 10:00"},
		},
		SectionBacklog: []backlogSeed{{Title: "Fix the porch light"}},
		SectionMemory:  []memorySeed{{Text: "Prefers tea."}},
	}})
	w.play(Step{Say: call("memory_add", `{"kind":"fact","text":"Lives upstairs."}`)})
	env := CheckEnv{Store: w.Store, Location: w.Location}

	check := func(checks ...any) []Failure {
		var store []json.RawMessage
		for _, c := range checks {
			store = append(store, raw(t, c))
		}
		return CheckStep(t.Context(), &Expect{Store: store}, TurnRecord{}, env)
	}
	type m = map[string]any
	passing := []any{
		m{"check": "jobs", "count": 2},
		m{"check": "jobs", "recurring": true, "count": 1},
		m{"check": "jobs", "name": "morning news"},
		m{"check": "jobs", "cron": "0 9 * * *", "prompt_contains": "NEWS"},
		m{"check": "jobs", "prompt_equals": "Summarise the news."},
		m{"check": "jobs", "at": "2026-03-10T10:00", "prompt_contains": "dentist"},
		m{"check": "jobs", "recurring": false, "min": 1, "max": 1},
		m{"check": "memory", "contains": "upstairs"},
		m{"check": "memory", "count": 2},
		m{"check": "memory", "contains": "tea", "count": 1},
		m{"check": "backlog", "count": 1, "open": 1},
	}
	for _, c := range passing {
		if failures := check(c); len(failures) != 0 {
			t.Fatalf("%v: %+v", c, failures)
		}
	}
	failing := map[string]any{
		"store.jobs":    m{"check": "jobs", "at": "2026-03-10T11:00"},
		"store.memory":  m{"check": "memory", "contains": "absent"},
		"store.backlog": m{"check": "backlog", "open": 0},
	}
	for name, c := range failing {
		failures := check(c)
		if len(failures) != 1 || failures[0].Check != name {
			t.Fatalf("%v: failures = %+v, want one %s", c, failures, name)
		}
	}
	if failures := check(m{"check": "jobs", "recurring": true, "count": 0}); len(failures) != 1 || !strings.Contains(failures[0].Message, "1 matching") {
		t.Fatalf("a recurring job exists: %+v", failures)
	}
}

func TestStoreCheckShapesAreValidatedByTheLoader(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"jobs without anything": `{"check":"jobs"}`,
		"jobs with a bad time":  `{"check":"jobs","at":"tomorrow"}`,
		"jobs count and max":    `{"check":"jobs","count":1,"max":2}`,
		"jobs unknown field":    `{"check":"jobs","title":"x"}`,
		"memory empty":          `{"check":"memory"}`,
		"memory negative":       `{"check":"memory","count":-1}`,
		"backlog empty":         `{"check":"backlog"}`,
		"backlog negative":      `{"check":"backlog","open":-1}`,
		"backlog unknown":       `{"check":"backlog","size":1}`,
		"jobs min above max":    `{"check":"jobs","min":3,"max":2}`,
	} {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var head struct{ Check string }
			if err := json.Unmarshal([]byte(body), &head); err != nil {
				t.Fatal(err)
			}
			if err := DefaultRegistry.checks[head.Check](json.RawMessage(body)); err == nil {
				t.Fatalf("%s accepted", body)
			}
		})
	}
	for _, body := range []string{`{"check":"jobs","recurring":false,"count":0}`, `{"check":"memory","kind":"fact"}`, `{"check":"backlog","count":2}`} {
		var head struct{ Check string }
		_ = json.Unmarshal([]byte(body), &head)
		if err := DefaultRegistry.checks[head.Check](json.RawMessage(body)); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
}
