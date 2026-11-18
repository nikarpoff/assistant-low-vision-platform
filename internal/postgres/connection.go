// Package postgres wraps the pgx connection pool of the journal database (CON-2).
//
// The repositories depend on small interfaces of their own with the methods of
// Conn they need, so they are tested with moq mocks instead of a database.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
)

// Pool settings of the journal: the writer is one goroutine and /v1/stats is rare.
const (
	maxConns       = 4
	connectTimeout = 5 * time.Second
)

// errParseDSN is returned for an APP_POSTGRES_DSN that is not a connection string.
// The parse error of pgx quotes the DSN, so it is not wrapped: the DSN holds the database password.
const errParseDSN = "failed to parse APP_POSTGRES_DSN: not a PostgreSQL connection string"

// Rows is the subset of pgx.Rows the repositories use; it keeps them mockable.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Conn is a pool of connections to PostgreSQL that exposes a subset of its methods.
type Conn struct {
	pool *pgxpool.Pool
}

// NewPostgresConnection creates a pool of connections to APP_POSTGRES_DSN.
//
// The pool connects lazily: the database is not contacted here, so an
// unavailable database does not stop the gateway (NFR-4). A DSN without
// connect_timeout gets a connect timeout of 5 s.
func NewPostgresConnection(vars *env.Vars) (*Conn, error) {
	config, err := pgxpool.ParseConfig(vars.AppPostgresDSN)
	if err != nil {
		return nil, errors.New(errParseDSN)
	}

	config.MaxConns = maxConns
	if config.ConnConfig.ConnectTimeout == 0 {
		config.ConnConfig.ConnectTimeout = connectTimeout
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres pool: %w", err)
	}

	return &Conn{pool: pool}, nil
}

// Exec executes a statement. Without arguments it goes by the simple protocol,
// so sql may hold several statements: PostgreSQL runs them in one implicit transaction.
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.pool.Exec(ctx, sql, args...)
	return err
}

// ExecBatch executes the queued statements of the batch in one transaction and returns the first error.
func (c *Conn) ExecBatch(ctx context.Context, batch *pgx.Batch) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, batch).Close()
	})
}

// Query executes a query and returns its rows; the caller closes them.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Close closes all connections of the pool.
func (c *Conn) Close() {
	c.pool.Close()
}
