package main

import (
	"path/filepath"
	"testing"
)

func TestExecutablePathResolvesPathsAndLeavesCommandNames(t *testing.T) {
	for setting, want := range map[string]string{"": "", "pi": "pi", "node": "node"} {
		if got, err := executablePath(setting); err != nil || got != want {
			t.Errorf("executablePath(%q) = %q, %v; want %q", setting, got, err, want)
		}
	}
	got, err := executablePath("./bin/pi")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "pi" {
		t.Errorf("executablePath(./bin/pi) = %q, want an absolute path", got)
	}
}
