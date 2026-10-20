package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadNASRootsFrom(t *testing.T, document string) error {
	t.Helper()
	file := filepath.Join(t.TempDir(), "roots.json")
	if err := os.WriteFile(file, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadNASRoots(file)
	return err
}

func TestNASRootsValidateOwnershipFlags(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			name: "valid own and shared roots",
			document: `{"principals":[
				{"principal_id":"alice","default_write":"inbox","roots":[
					{"name":"home","path":"/mnt/alice"},
					{"name":"inbox","path":"/mnt/alice-inbox","writable":true,"create_folders":true},
					{"name":"shared","path":"/mnt/shared","shared":true}]},
				{"principal_id":"bob","roots":[{"name":"shared","path":"/mnt/shared","shared":true}]}]}`,
		},
		{
			name:     "create_folders on a read-only root",
			document: `{"principals":[{"principal_id":"alice","roots":[{"name":"home","path":"/mnt/a","create_folders":true}]}]}`,
			want:     "create_folders on a read-only root",
		},
		{
			name: "shared default_write",
			document: `{"principals":[{"principal_id":"alice","default_write":"shared-inbox","roots":[
				{"name":"shared-inbox","path":"/mnt/s","writable":true,"shared":true}]}]}`,
			want: "shared default_write",
		},
		{
			name: "path granted twice without shared",
			document: `{"principals":[
				{"principal_id":"alice","roots":[{"name":"shared","path":"/mnt/shared","shared":true}]},
				{"principal_id":"bob","roots":[{"name":"shared","path":"/mnt/shared"}]}]}`,
			want: "must be marked shared in every grant",
		},
		{
			name: "path nested inside another principal's root",
			document: `{"principals":[
				{"principal_id":"alice","roots":[{"name":"home","path":"/mnt/homes"}]},
				{"principal_id":"bob","roots":[{"name":"home","path":"/mnt/homes/bob"}]}]}`,
			want: "must be marked shared in every grant",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := loadNASRootsFrom(t, test.document)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
