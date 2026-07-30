// Command migrate validates and applies the project's SQL migrations.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	projectmigration "github.com/aigateway-lab/ai-gateway-platform/internal/migration"
	gomigrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

const usage = `Usage:
  go run ./cmd/migrate validate [--path migrations]
  go run ./cmd/migrate up [--path migrations]
  go run ./cmd/migrate down [--path migrations] [--steps 1] --confirm-development
  go run ./cmd/migrate version [--path migrations]

DATABASE_URL is required for up, down, and version. The connection string is never logged.
Down migrations are rejected when APP_ENV=production and require explicit confirmation.`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "migration error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		if err := writeLine(stderr, usage); err != nil {
			return err
		}
		return errors.New("a migration command is required")
	}

	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "up":
		return runUp(args[1:], stdout, stderr)
	case "down":
		return runDown(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		return writeLine(stdout, usage)
	default:
		if err := writeLine(stderr, usage); err != nil {
			return err
		}
		return fmt.Errorf("unknown migration command %q", args[0])
	}
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("path", "migrations", "migration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("validate accepts no positional arguments")
	}

	sets, err := projectmigration.ValidateDir(*path)
	if err != nil {
		return err
	}
	last := sets[len(sets)-1]
	return writeFormat(stdout, "migration validation passed: count=%d latest=%06d_%s\n", len(sets), last.Version, last.Name)
}

func runUp(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("path", "migrations", "migration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("up accepts no positional arguments")
	}

	return withMigrator(*path, func(engine *gomigrate.Migrate) error {
		err := engine.Up()
		if errors.Is(err, gomigrate.ErrNoChange) {
			return writeLine(stdout, "database already at latest migration; no change")
		}
		if err != nil {
			return fmt.Errorf("apply up migrations: %w", err)
		}
		return writeLine(stdout, "database migrated to latest version")
	})
}

func runDown(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("down", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("path", "migrations", "migration directory")
	steps := flags.Int("steps", 1, "number of migrations to roll back")
	confirmed := flags.Bool("confirm-development", false, "confirm a development-only rollback")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("down accepts no positional arguments")
	}
	if !*confirmed {
		return errors.New("down requires --confirm-development")
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		return errors.New("down migrations are disabled when APP_ENV=production")
	}
	if *steps < 1 || *steps > 100 {
		return errors.New("steps must be between 1 and 100")
	}

	return withMigrator(*path, func(engine *gomigrate.Migrate) error {
		err := engine.Steps(-*steps)
		if errors.Is(err, gomigrate.ErrNoChange) {
			return writeLine(stdout, "database has no applied migration to roll back; no change")
		}
		if err != nil {
			return fmt.Errorf("roll back %d migration(s): %w", *steps, err)
		}
		return writeFormat(stdout, "rolled back %d migration(s)\n", *steps)
	})
}

func runVersion(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("path", "migrations", "migration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("version accepts no positional arguments")
	}

	return withMigrator(*path, func(engine *gomigrate.Migrate) error {
		version, dirty, err := engine.Version()
		if errors.Is(err, gomigrate.ErrNilVersion) {
			return writeLine(stdout, "migration version=0 dirty=false")
		}
		if err != nil {
			return fmt.Errorf("read migration version: %w", err)
		}
		return writeFormat(stdout, "migration version=%d dirty=%t\n", version, dirty)
	})
}

func withMigrator(path string, action func(*gomigrate.Migrate) error) (err error) {
	if _, err := projectmigration.ValidateDir(path); err != nil {
		return err
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	sourceURL, err := projectmigration.FileSourceURL(path)
	if err != nil {
		return err
	}
	engine, err := gomigrate.New(sourceURL, databaseURL)
	if err != nil {
		return fmt.Errorf("initialize migration engine: %w", err)
	}
	defer func() {
		sourceErr, databaseErr := engine.Close()
		closeErr := errors.Join(sourceErr, databaseErr)
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close migration engine: %w", closeErr)
		}
	}()
	return action(engine)
}

func writeLine(writer io.Writer, value string) error {
	if _, err := fmt.Fprintln(writer, value); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}

func writeFormat(writer io.Writer, format string, values ...any) error {
	if _, err := fmt.Fprintf(writer, format, values...); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}
