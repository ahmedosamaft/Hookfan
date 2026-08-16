// Package store owns all database access: the pgx pool, embedded migrations,
// and the plain-SQL queries used by the rest of the gateway.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxParse validates a DSN and returns a pgx connection config. Shared by the
// pool and by the migration runner's database/sql bridge so both reject a
// malformed DATABASE_URL identically.
func pgxParse(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	return cfg, nil
}

type Store struct {
	pool *pgxpool.Pool
}

// New opens the connection pool and verifies it can reach the database.
func New(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool for packages that need to run their own
// queries or transactions.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// PlannerLag returns the age of the oldest event that has not yet had its
// delivery set created. This is the gateway's most important health signal: if
// the planner wedges, ingest keeps returning 200 to the provider while nothing
// is forwarded, which is otherwise silent. Returns 0 when nothing is pending.
func (s *Store) PlannerLag(ctx context.Context) (time.Duration, error) {
	var seconds float64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(received_at))), 0)
		  FROM events
		 WHERE planned_at IS NULL`).Scan(&seconds)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
