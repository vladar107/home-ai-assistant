package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A NAS operating system update can reset mounted secret files to mode 0777 under an access
// control list that still admits only their owner, so their mode bits must not stop the start.
func TestSecretFilesLoadWhateverTheirModeBits(t *testing.T) {
	readers := map[string]func(string) (string, error){
		"NS API key":           readNSAPIKeyFile,
		"Home Assistant token": readHATokenFile,
		"Grafana token":        readGrafanaTokenFile,
		"TypeSafe key":         readTypeSafeKeyFile,
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte("value-1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
			got, err := read(path)
			if err != nil || got != "value-1" {
				t.Fatalf("read = %q, %v; want the file's value", got, err)
			}
		})
	}
}
