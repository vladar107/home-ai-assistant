package eval

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func testRegistry() *Registry {
	r := NewRegistry()
	r.RegisterWorldSection("sample", func(raw json.RawMessage) error {
		var v struct {
			Items []string `json:"items"`
		}
		return json.Unmarshal(raw, &v)
	})
	r.RegisterStoreCheck("sample-check", func(json.RawMessage) error { return nil })
	return r
}

func TestRepositoryCasesLoad(t *testing.T) {
	if _, err := LoadCases(filepath.Join("..", "..", "evals", "cases")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadValidCaseAppliesDefaults(t *testing.T) {
	cases, err := testRegistry().LoadCases(filepath.Join("testdata", "valid"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	c := cases[0]
	if c.ID != "sample-reminder" || c.Kind != "wrong-local-time" || len(c.Steps) != 2 {
		t.Fatalf("unexpected case: %+v", c)
	}
	if c.Trials != DefaultTrials {
		t.Errorf("trials = %d, want default %d", c.Trials, DefaultTrials)
	}
	if got := c.Steps[0].Expect.Calls[0].Min; got == nil || *got != 1 {
		t.Errorf("call min = %v, want default 1", got)
	}
	at, err := c.Clock.Time()
	if err != nil || at.Format("2006-01-02 15:04 MST") != "2026-03-10 08:30 CET" {
		t.Errorf("clock = %v, %v", at, err)
	}
	if !strings.HasSuffix(c.File, "full.json") {
		t.Errorf("file = %q", c.File)
	}
}

func TestLoadRejectsInvalidCases(t *testing.T) {
	tests := []struct {
		dir     string
		file    string
		message string
	}{
		{"malformed", "case.json", "decode"},
		{"duplicate-id", "b.json", `duplicate case id "same-id"`},
		{"unknown-field", "case.json", `unknown field "turns"`},
		{"unknown-world", "case.json", "world.nonexistent: unknown world section"},
		{"bad-regexp", "case.json", "steps[0]: expect.reply.matches[0]"},
		{"bad-not-matches", "case.json", "steps[0]: expect.reply.not_matches[0]"},
		{"confirm-first", "case.json", "steps[0]: confirm cannot be the first step"},
		{"two-actions", "case.json", "steps[0]: want exactly one action"},
		{"bad-zone", "case.json", `clock.zone "Mars/Olympus"`},
		{"bad-user-language", "case.json", `user.language "English please"`},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			_, err := testRegistry().LoadCases(filepath.Join("testdata", tt.dir))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.file) {
				t.Errorf("error %q does not name %s", err, tt.file)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Errorf("error %q does not contain %q", err, tt.message)
			}
		})
	}
}

func TestWorldValidatorErrorNamesSection(t *testing.T) {
	r := NewRegistry()
	r.RegisterWorldSection("sample", func(json.RawMessage) error { return errors.New("bad shape") })
	r.RegisterStoreCheck("sample-check", func(json.RawMessage) error { return nil })
	_, err := r.LoadCases(filepath.Join("testdata", "valid"))
	if err == nil || !strings.Contains(err.Error(), "world.sample: bad shape") || !strings.Contains(err.Error(), "full.json") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownStoreCheckIsRejected(t *testing.T) {
	r := NewRegistry()
	r.RegisterWorldSection("sample", func(json.RawMessage) error { return nil })
	_, err := r.LoadCases(filepath.Join("testdata", "valid"))
	if err == nil || !strings.Contains(err.Error(), `unknown check "sample-check"`) {
		t.Fatalf("got %v", err)
	}
}
