package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationFS embed.FS

// advisoryLockID is an arbitrary but fixed key. All replicas contend on this
// one lock so migrations can never run concurrently.
//
// goose ships its own session locker, but holding the lock ourselves keeps it
// around the whole run — including the version check — so a replica can never
// observe a partially applied schema and decide it has nothing to do.
const advisoryLockID int64 = 0x484F4F4B_46414E01

// newProvider builds a goose provider over the embedded migrations.
//
// The Provider API is used rather than the package-level goose.Up/Down helpers
// because it keeps no global dialect state and returns structured results
// instead of writing to a logger.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		return nil, fmt.Errorf("init migration provider: %w", err)
	}
	return p, nil
}

// withMigrationLock runs fn while holding the advisory lock on a single
// dedicated connection. pg_advisory_lock is session-scoped, so a pooled
// connection could be recycled mid-run and silently drop the lock.
func withMigrationLock(ctx context.Context, db *sql.DB, log *slog.Logger, fn func(*goose.Provider) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	start := time.Now()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if waited := time.Since(start); waited > time.Second {
		log.Info("waited for migration lock held by another replica", "waited", waited.Round(time.Millisecond))
	}
	defer func() {
		// context.Background: the lock must be released even if ctx is done.
		if _, err := conn.ExecContext(context.Background(),
			`SELECT pg_advisory_unlock($1)`, advisoryLockID); err != nil {
			log.Warn("release migration lock", "error", err)
		}
	}()

	p, err := newProvider(db)
	if err != nil {
		return err
	}
	defer p.Close()

	return fn(p)
}

// Migrate applies every pending migration inside the advisory lock. Replicas
// that lose the race block here until the leader finishes, then find nothing
// to apply — they never start serving against a half-migrated schema.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	db, err := openSQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	return withMigrationLock(ctx, db, log, func(p *goose.Provider) error {
		results, err := p.Up(ctx)
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		for _, r := range results {
			log.Info("migration applied",
				"version", r.Source.Version,
				"name", r.Source.Path,
				"duration", r.Duration.Round(time.Millisecond))
		}

		version, err := p.GetDBVersion(ctx)
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if len(results) == 0 {
			log.Info("schema up to date", "version", version)
		} else {
			log.Info("migrations complete", "applied", len(results), "version", version)
		}
		return nil
	})
}

// MigrateDown rolls back the most recent migration. Destructive, so it is only
// reachable through `hookfan migrate down` and never runs at startup.
func MigrateDown(ctx context.Context, dsn string, log *slog.Logger) error {
	db, err := openSQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	return withMigrationLock(ctx, db, log, func(p *goose.Provider) error {
		result, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("roll back migration: %w", err)
		}
		version, err := p.GetDBVersion(ctx)
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		log.Info("rollback complete",
			"rolled_back", result.Source.Version,
			"version", version)
		return nil
	})
}

// MigrationStatus writes each migration and its applied state to stdout.
func MigrationStatus(ctx context.Context, dsn string) error {
	db, err := openSQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	p, err := newProvider(db)
	if err != nil {
		return err
	}
	defer p.Close()

	statuses, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}

	fmt.Printf("%-10s %-30s %s\n", "VERSION", "MIGRATION", "APPLIED")
	for _, s := range statuses {
		applied := "pending"
		if s.State == goose.StateApplied {
			applied = "yes"
			if !s.AppliedAt.IsZero() {
				applied = s.AppliedAt.Format(time.RFC3339)
			}
		}
		fmt.Printf("%-10d %-30s %s\n", s.Source.Version, s.Source.Path, applied)
	}
	return nil
}

// openSQL opens a database/sql handle backed by pgx. goose needs a
// database/sql connection; the rest of the gateway uses pgx directly through
// the pool, and stdlib bridges the two without a second driver dependency.
func openSQL(dsn string) (*sql.DB, error) {
	cfg, err := pgxParse(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	return db, nil
}
