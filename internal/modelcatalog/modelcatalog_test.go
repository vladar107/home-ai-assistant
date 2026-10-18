package modelcatalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const store = `{
  "test-provider": {
    "checkedAt": 1,
    "models": [
      {"id": "acme-5.4", "name": "Acme 5.4", "reasoning": true, "contextWindow": 272000,
       "cost": {"input": 2.5, "output": 15},
       "thinkingLevelMap": {"xhigh": "xhigh", "minimal": "low"}},
      {"id": "acme-5.6-sol", "name": "Acme 5.6 Sol", "reasoning": true, "contextWindow": 272000,
       "cost": {"input": 5, "output": 30},
       "thinkingLevelMap": {"xhigh": "xhigh", "max": "max", "minimal": "low"}},
      {"id": "acme-6-sol", "name": "Acme 6 Sol", "reasoning": true, "contextWindow": 1000000,
       "cost": {"input": 5, "output": 30},
       "thinkingLevelMap": {"off": null, "minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}},
      {"id": "acme-6.1-sol", "name": "Acme 6.1 Sol", "reasoning": true, "contextWindow": 272000,
       "cost": {"input": 5, "output": 30},
       "thinkingLevelMap": {"off": null, "xhigh": "xhigh"}},
      {"id": "acme-6-lite", "name": "Acme 6 Lite", "reasoning": true, "contextWindow": 128000,
       "cost": {"input": 0.2, "output": 1.2},
       "thinkingLevelMap": {"minimal": "low", "max": "max"}},
      {"id": "acme-plain", "name": "Acme Plain", "reasoning": false, "contextWindow": 8000,
       "cost": {"input": 1, "output": 2}}
    ]
  },
  "other-provider": {"models": [{"id": "other-1", "name": "Other", "reasoning": true}]}
}`

func catalogWith(t *testing.T, contents string) (*modelcatalog.Catalog, string) {
	t.Helper()
	dir := t.TempDir()
	profile := filepath.Join(dir, "alice")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if contents != "" {
		if err := os.WriteFile(filepath.Join(profile, "models-store.json"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return modelcatalog.New(dir, "test-provider"), dir
}

func TestModelsListOnlyTheConfiguredProviderWithTheirLevels(t *testing.T) {
	catalog, _ := catalogWith(t, store)
	models, err := catalog.Models(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"acme-5.4":     {"off", "low", "medium", "high", "xhigh"},
		"acme-5.6-sol": {"off", "low", "medium", "high", "xhigh", "max"},
		"acme-6-sol":   {"low", "medium", "high", "xhigh", "max"},
		"acme-6.1-sol": {"minimal", "low", "medium", "high", "xhigh"},
		"acme-6-lite":  {"off", "low", "medium", "high", "max"},
		"acme-plain":   {"off"},
	}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d: %+v", len(models), len(want), models)
	}
	for _, m := range models {
		levels, ok := want[m.ID]
		if !ok {
			t.Errorf("unexpected model %s", m.ID)
			continue
		}
		if !reflect.DeepEqual(m.Efforts, levels) {
			t.Errorf("%s efforts = %v, want %v", m.ID, m.Efforts, levels)
		}
	}
	if !models[0].Supports("xhigh") || models[0].Supports("max") {
		t.Errorf("Supports disagrees with the efforts of %s", models[0].ID)
	}
}

func TestModelsAreUnavailableWhenTheCatalogCannotBeUsed(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		provider string
	}{
		{"missing file", "", "test-provider"},
		{"partial file", `{"test-provider": {"models": [{"id": "acme-`, "test-provider"},
		{"not an object", `[]`, "test-provider"},
		{"provider absent", store, "absent-provider"},
		{"empty model list", `{"test-provider": {"models": []}}`, "test-provider"},
		{"no models key", `{"test-provider": {"checkedAt": 1}}`, "test-provider"},
		{"models of the wrong type", `{"test-provider": {"models": "none"}}`, "test-provider"},
		{"models without ids", `{"test-provider": {"models": [{"name": "nameless"}]}}`, "test-provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, dir := catalogWith(t, tc.contents)
			catalog := modelcatalog.New(dir, tc.provider)
			if _, err := catalog.Models(context.Background(), "alice"); !errors.Is(err, modelcatalog.ErrUnavailable) {
				t.Errorf("error = %v, want ErrUnavailable", err)
			}
		})
	}
	catalog, _ := catalogWith(t, store)
	if _, err := catalog.Models(context.Background(), "../escape"); !errors.Is(err, modelcatalog.ErrUnavailable) {
		t.Errorf("unsafe principal: error = %v, want ErrUnavailable", err)
	}
}

func TestModelsWaitForPiToReleaseTheLock(t *testing.T) {
	catalog, dir := catalogWith(t, store)
	lock := filepath.Join(dir, "alice", "models-store.json.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.Remove(lock)
	}()
	if _, err := catalog.Models(context.Background(), "alice"); err != nil {
		t.Fatalf("lock released within the wait: %v", err)
	}
}

func TestResolveTakesIDsAndFamilyNames(t *testing.T) {
	catalog, _ := catalogWith(t, store)
	models, err := catalog.Models(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"acme-5.4":     "acme-5.4",
		"ACME-5.6-SOL": "acme-5.6-sol",
		"sol":          "acme-6.1-sol",
		"lite":         "acme-6-lite",
	} {
		got, err := modelcatalog.Resolve(models, name)
		if err != nil || got.ID != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", name, got.ID, err, want)
		}
	}
	for _, name := range []string{"missing", ""} {
		_, err := modelcatalog.Resolve(models, name)
		var resolveErr *modelcatalog.ResolveError
		if !errors.As(err, &resolveErr) || resolveErr.Ambiguous || len(resolveErr.Options) != len(models) {
			t.Errorf("Resolve(%q) error = %v", name, err)
		}
	}
	tied := []modelcatalog.Model{{ID: "alpha-2-sol"}, {ID: "beta-2-sol"}, {ID: "alpha-1-sol"}}
	_, err = modelcatalog.Resolve(tied, "sol")
	var resolveErr *modelcatalog.ResolveError
	if !errors.As(err, &resolveErr) || !resolveErr.Ambiguous || !reflect.DeepEqual(resolveErr.Options, []string{"alpha-2-sol", "beta-2-sol"}) {
		t.Errorf("tied versions: error = %v", err)
	}
}

func TestDescribeNamesContextAndRelativeCost(t *testing.T) {
	catalog, _ := catalogWith(t, store)
	models, err := catalog.Models(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	lines := modelcatalog.Describe(models)
	want := map[string]string{
		"acme-5.4":    "Acme 5.4, 272k context, 12.5x cost",
		"acme-6-sol":  "Acme 6 Sol, 1M context, 25x cost",
		"acme-6-lite": "Acme 6 Lite, 128k context, lowest cost",
	}
	for i, m := range models {
		if line, ok := want[m.ID]; ok && lines[i] != line {
			t.Errorf("%s: %q, want %q", m.ID, lines[i], line)
		}
	}
}

func TestFallbackOffersOnlyTheDefaults(t *testing.T) {
	models := modelcatalog.Fallback(
		agentruntime.ModelEffort{Model: "acme-a", Effort: "low"},
		agentruntime.ModelEffort{Model: "acme-b", Effort: "high"},
		agentruntime.ModelEffort{Model: "acme-a", Effort: "high"},
	)
	if len(models) != 2 || models[0].ID != "acme-a" || !reflect.DeepEqual(models[0].Efforts, []string{"low", "high"}) || models[1].ID != "acme-b" {
		t.Errorf("fallback = %+v", models)
	}
}

func TestSupportsAcceptsPiAliasesAndCanonicalNamesTheEffort(t *testing.T) {
	catalog, _ := catalogWith(t, store)
	models, err := catalog.Models(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	m, err := modelcatalog.Resolve(models, "acme-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m.Efforts, "minimal") {
		t.Fatalf("the list for display holds the alias: %v", m.Efforts)
	}
	if !m.Supports("minimal") || !m.Supports("low") {
		t.Error("minimal and low are both accepted")
	}
	if got, ok := m.Canonical("minimal"); !ok || got != "low" {
		t.Errorf("Canonical(minimal) = %q, %v, want low", got, ok)
	}
	if got, ok := m.Canonical("high"); !ok || got != "high" {
		t.Errorf("Canonical(high) = %q, %v", got, ok)
	}
	if m.Supports("max") {
		t.Error("max is hidden on this model")
	}
	if _, ok := m.Canonical("max"); ok {
		t.Error("Canonical accepted an unsupported level")
	}
}

func TestFallbackOffersTheUnionOfEffortsForOneModel(t *testing.T) {
	models := modelcatalog.Fallback(
		agentruntime.ModelEffort{Model: "acme-a", Effort: "high"},
		agentruntime.ModelEffort{Model: "acme-a", Effort: "low"},
		agentruntime.ModelEffort{Model: "acme-a", Effort: "high"},
	)
	if len(models) != 1 || !reflect.DeepEqual(models[0].Efforts, []string{"low", "high"}) {
		t.Errorf("fallback = %+v", models)
	}
	if !models[0].Supports("low") || models[0].Supports("max") {
		t.Error("fallback support does not follow the union")
	}
}

func TestModelsAreUnavailableWhenTheLockOutlastsTheWait(t *testing.T) {
	catalog, dir := catalogWith(t, store)
	if err := os.Mkdir(filepath.Join(dir, "alice", "models-store.json.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := catalog.Models(context.Background(), "alice"); !errors.Is(err, modelcatalog.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("waited %v", elapsed)
	}
}

func TestModelsReclaimAStaleLock(t *testing.T) {
	catalog, dir := catalogWith(t, store)
	lock := filepath.Join(dir, "alice", "models-store.json.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Models(context.Background(), "alice"); err != nil {
		t.Fatalf("stale lock was not reclaimed: %v", err)
	}
}
