// Package database holds the schema and applies migrations.
package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is an arbitrary constant: nodes starting together take turns migrating.
const migrationLock = 7302117

// Migrate applies any migrations not yet recorded, each in its own transaction, in filename order.
func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if directive(string(body), "no-transaction") != "" {
			err = applyOutsideTransaction(ctx, conn, name, string(body))
		} else {
			err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, string(body)); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
				return err
			})
		}
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// directive finds a line "-- migrate:name value" in a migration. A directive with no value reports its own name.
func directive(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "-- migrate:"+name)
		if !ok {
			continue
		}
		if value := strings.TrimSpace(rest); value != "" {
			return value
		}
		return name
	}
	return ""
}

// applyOutsideTransaction runs a migration that cannot be in a transaction, which holds one statement. An interrupted
// CREATE INDEX CONCURRENTLY leaves an invalid index that IF NOT EXISTS would then accept, so one of those goes first.
func applyOutsideTransaction(ctx context.Context, conn *pgxpool.Conn, name, body string) error {
	if index := directive(body, "concurrent-index"); index != "" {
		var invalid bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass($1) AND NOT indisvalid)`, index).Scan(&invalid)
		if err != nil {
			return err
		}
		if invalid {
			if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY `+pgx.Identifier{index}.Sanitize()); err != nil {
				return err
			}
		}
	}
	if _, err := conn.Exec(ctx, body); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
	return err
}

// Latest names the newest migration in this binary. A worker waits for it, since only the server migrates.
func Latest() (string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return "", err
	}
	latest := ""
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() > latest {
			latest = e.Name()
		}
	}
	return latest, nil
}
