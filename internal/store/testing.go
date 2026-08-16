package store

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestStore connects to the database named by TEST_DATABASE_URL, applies
// migrations, and truncates every table so each test starts from a known
// state. Tests that need a database skip when the variable is unset, so
// `go test ./...` still works without one.
//
// Each test package gets its own Postgres schema. `go test ./...` runs
// different packages concurrently, and a single shared schema means one
// package's TRUNCATE wipes another's fixtures mid-test.
func TestStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test (run: make test-integration)")
	}

	schema := testSchemaName()
	// search_path is set on every pooled connection, so all unqualified
	// queries — including the migrations — land in this package's schema.
	scoped := withSearchPath(dsn, schema)

	ctx := context.Background()
	if err := createSchema(ctx, dsn, schema); err != nil {
		t.Fatalf("create test schema %s: %v", schema, err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := Migrate(ctx, scoped, log); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	s, err := New(ctx, scoped, 8)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(s.Close)

	if _, err := s.pool.Exec(ctx, `
		TRUNCATE deliveries, events, subscriptions, services, listeners
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	return s
}

// testSchemaName derives a stable schema name from the package under test, so
// concurrent packages never share tables.
func testSchemaName() string {
	// t.Name() is the test, not the package; the binary's import path is what
	// distinguishes packages, and it is available through the executable name.
	pkg := "pkg"
	if wd, err := os.Getwd(); err == nil {
		pkg = filepath.Base(wd)
	}
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '_'
	}, pkg)
	return "test_" + clean
}

func createSchema(ctx context.Context, dsn, schema string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	// Dropped and recreated so a previous run's schema never leaks in.
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `CREATE SCHEMA `+schema)
	return err
}

// withSearchPath appends a search_path option to the connection string.
func withSearchPath(dsn, schema string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}
