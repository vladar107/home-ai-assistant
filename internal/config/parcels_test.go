package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParcelSourcesAreParsedAndHeldToTheAllowlist(t *testing.T) {
	t.Setenv("HOME_AGENT_PARCEL_SOURCES", " alice = entry1, entry2 ; bob=entry3;")
	var cfg ParcelsConfig
	if err := loadParcelsConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"alice": {"entry1", "entry2"}, "bob": {"entry3"}}
	if !reflect.DeepEqual(cfg.Sources, want) {
		t.Fatalf("sources = %v", cfg.Sources)
	}
	if err := cfg.validate([]string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate([]string{"alice"}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("principal off the allowlist: %v", err)
	}
	if err := (ParcelsConfig{Sources: map[string][]string{"alice": {"shared"}, "bob": {"shared"}}}).validate(nil); err == nil || !strings.Contains(err.Error(), "assigned to both") {
		t.Fatalf("entry for two principals: %v", err)
	}

	for _, value := range []string{"alice", "alice=", "alice=bad entry", "alice=a;alice=b", "alice=e1,e1", "=entry1"} {
		t.Setenv("HOME_AGENT_PARCEL_SOURCES", value)
		if err := loadParcelsConfig(&ParcelsConfig{}); err == nil || !strings.Contains(err.Error(), "HOME_AGENT_PARCEL_SOURCES") {
			t.Fatalf("%q: %v", value, err)
		}
	}

	t.Setenv("HOME_AGENT_PARCEL_SOURCES", "")
	cfg = ParcelsConfig{}
	if err := loadParcelsConfig(&cfg); err != nil || len(cfg.Sources) != 0 {
		t.Fatalf("empty = %+v %v", cfg, err)
	}
}
