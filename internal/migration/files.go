// Package migration validates and locates the project's SQL migrations.
package migration

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
)

var migrationFilePattern = regexp.MustCompile(`^([0-9]{6})_([a-z][a-z0-9_]*)\.(up|down)\.sql$`)

// FileSet is one paired up/down migration.
type FileSet struct {
	Version  uint
	Name     string
	UpPath   string
	DownPath string
}

// ValidateDir enforces deterministic, paired, contiguous migrations.
func ValidateDir(dir string) ([]FileSet, error) {
	migrationFS := os.DirFS(dir)
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration directory: %w", err)
	}

	byVersion := make(map[uint]*FileSet)
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("migration directory must be flat; found directory %q", entry.Name())
		}
		if filepath.Ext(entry.Name()) != ".sql" {
			continue
		}

		matches := migrationFilePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q; expected NNNNNN_name.up.sql or NNNNNN_name.down.sql", entry.Name())
		}

		parsedVersion, err := strconv.ParseUint(matches[1], 10, 32)
		if err != nil || parsedVersion == 0 {
			return nil, fmt.Errorf("migration %q must use a positive six-digit version", entry.Name())
		}
		version := uint(parsedVersion)
		name := matches[2]
		direction := matches[3]

		contents, err := fs.ReadFile(migrationFS, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if len(bytes.TrimSpace(contents)) == 0 {
			return nil, fmt.Errorf("migration %q must not be empty", entry.Name())
		}

		pair, exists := byVersion[version]
		if !exists {
			pair = &FileSet{Version: version, Name: name}
			byVersion[version] = pair
		} else if pair.Name != name {
			return nil, fmt.Errorf("migration version %06d has conflicting names %q and %q", version, pair.Name, name)
		}

		path := filepath.Join(dir, entry.Name())
		switch direction {
		case "up":
			if pair.UpPath != "" {
				return nil, fmt.Errorf("migration version %06d has more than one up file", version)
			}
			pair.UpPath = path
		case "down":
			if pair.DownPath != "" {
				return nil, fmt.Errorf("migration version %06d has more than one down file", version)
			}
			pair.DownPath = path
		}
	}

	if len(byVersion) == 0 {
		return nil, fmt.Errorf("migration directory %q contains no SQL migrations", dir)
	}

	versions := make([]int, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, int(version))
	}
	sort.Ints(versions)

	sets := make([]FileSet, 0, len(versions))
	for index, rawVersion := range versions {
		version := uint(rawVersion)
		expected := uint(index + 1)
		if version != expected {
			return nil, fmt.Errorf("migration versions must be contiguous from 000001; expected %06d, found %06d", expected, version)
		}
		pair := byVersion[version]
		if pair.UpPath == "" || pair.DownPath == "" {
			return nil, fmt.Errorf("migration %06d_%s must have both up and down files", version, pair.Name)
		}
		sets = append(sets, *pair)
	}

	return sets, nil
}

// FileSourceURL returns the file:// URL expected by golang-migrate.
func FileSourceURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve migration directory: %w", err)
	}
	slashPath := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" {
		// golang-migrate's file driver concatenates URL host/path and passes the
		// result to os.DirFS. An RFC-style file:///C:/ path retains the leading
		// slash and is not a valid Windows volume path, so use an opaque file URL.
		return (&url.URL{Scheme: "file", Opaque: slashPath}).String(), nil
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String(), nil
}
