// Package db opens the Postgres connection pool and applies migrations.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/migrations"
)

// Connect opens a pool and checks that the database answers.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// migrationLockID is an arbitrary number used for a Postgres advisory lock,
// so two processes running migrations at once don't both apply a file.
const migrationLockID = 727274

// Migrate applies every migrations/*.sql file that has not been applied yet,
// in filename order. Each file runs in its own transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		did, err := applyOne(ctx, pool, name, version)
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", name, err)
		}
		if did {
			applied = append(applied, version)
		}
	}
	return applied, nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, name, version string) (bool, error) {
	sql, err := migrations.FS.ReadFile(name)
	if err != nil {
		return false, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) // does nothing if Commit already succeeded

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return false, err
	}

	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	// No arguments, so pgx uses the simple protocol, which allows a file
	// with several SQL statements.
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Querier is satisfied by both a pool and a transaction, so read helpers can
// be used inside or outside a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
