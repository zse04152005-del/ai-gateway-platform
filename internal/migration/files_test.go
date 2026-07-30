package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		files     map[string]string
		wantCount int
		wantError string
	}{
		{
			name: "valid paired migrations",
			files: map[string]string{
				"000001_create_schema.up.sql":   "CREATE SCHEMA app;",
				"000001_create_schema.down.sql": "DROP SCHEMA app;",
				"000002_add_table.up.sql":       "CREATE TABLE app.items(id bigint);",
				"000002_add_table.down.sql":     "DROP TABLE app.items;",
				"README.md":                     "documentation",
			},
			wantCount: 2,
		},
		{
			name: "missing pair",
			files: map[string]string{
				"000001_create_schema.up.sql": "CREATE SCHEMA app;",
			},
			wantError: "must have both up and down files",
		},
		{
			name: "version gap",
			files: map[string]string{
				"000001_first.up.sql":   "SELECT 1;",
				"000001_first.down.sql": "SELECT 1;",
				"000003_third.up.sql":   "SELECT 1;",
				"000003_third.down.sql": "SELECT 1;",
			},
			wantError: "expected 000002, found 000003",
		},
		{
			name: "conflicting names",
			files: map[string]string{
				"000001_first.up.sql":    "SELECT 1;",
				"000001_second.down.sql": "SELECT 1;",
			},
			wantError: "conflicting names",
		},
		{
			name: "invalid filename",
			files: map[string]string{
				"1_schema.up.sql": "SELECT 1;",
			},
			wantError: "invalid migration filename",
		},
		{
			name: "empty SQL",
			files: map[string]string{
				"000001_schema.up.sql":   " \n\t",
				"000001_schema.down.sql": "SELECT 1;",
			},
			wantError: "must not be empty",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, contents := range test.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			sets, err := ValidateDir(dir)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("ValidateDir() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateDir() unexpected error: %v", err)
			}
			if len(sets) != test.wantCount {
				t.Fatalf("ValidateDir() count = %d, want %d", len(sets), test.wantCount)
			}
		})
	}
}

func TestValidateDirRejectsSubdirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	if _, err := ValidateDir(dir); err == nil || !strings.Contains(err.Error(), "must be flat") {
		t.Fatalf("ValidateDir() error = %v, want flat-directory error", err)
	}
}

func TestFileSourceURL(t *testing.T) {
	t.Parallel()
	got, err := FileSourceURL(t.TempDir())
	if err != nil {
		t.Fatalf("FileSourceURL() error: %v", err)
	}
	if !strings.HasPrefix(got, "file://") {
		if !strings.HasPrefix(got, "file:") {
			t.Fatalf("FileSourceURL() = %q, want file URL", got)
		}
	}
}
