package database

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDirective(t *testing.T) {
	body := "-- migrate:no-transaction\n-- migrate:concurrent-index events_ingress\nCREATE INDEX x ON y (z);\n"
	if got := directive(body, "no-transaction"); got != "no-transaction" {
		t.Error(got)
	}
	if got := directive(body, "concurrent-index"); got != "events_ingress" {
		t.Error(got)
	}
	if got := directive("CREATE TABLE x (y int);", "no-transaction"); got != "" {
		t.Error(got)
	}
}

// freshDatabase makes a database of its own, migrated.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("STREAMKIT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("STREAMKIT_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("streamkit_migrate_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrationsCanRunTwice(t *testing.T) {
	db := freshDatabase(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

func indexIsValid(t *testing.T, db *pgxpool.Pool, name string) bool {
	t.Helper()
	var valid bool
	err := db.QueryRow(context.Background(), `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)`, name).Scan(&valid)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return valid
}

// A concurrent index build that is interrupted leaves an index that cannot be used, and IF NOT EXISTS would accept it.
func TestAnInterruptedIndexBuildIsDoneAgain(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()
	if !indexIsValid(t, db, "events_ingress") {
		t.Fatal("the index should be usable")
	}
	if _, err := db.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass('events_ingress')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '0005_events_ingress_index.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !indexIsValid(t, db, "events_ingress") {
		t.Fatal("the index should have been built again")
	}
}

// With nothing but the job in the query, counting what is running should not have to read every recording ever made.
func TestCountingRunningJobsUsesAnIndex(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO egresses (id, project_id, room_name, type, status)
		SELECT 'EG_' || i, 'p', 'r', 'room_composite', 'complete' FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `ANALYZE egresses`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, `EXPLAIN SELECT count(*) FROM egresses WHERE status IN ('starting', 'active', 'ending')`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(plan, "\n"); strings.Contains(text, "Seq Scan") {
		t.Fatalf("a sequential scan:\n%s", text)
	}
}
