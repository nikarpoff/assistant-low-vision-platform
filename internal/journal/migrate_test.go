package journal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/postgres"
	"github.com/nikarpoff/assistant-low-vision-platform/migrations"
)

func TestMigrate(t *testing.T) {
	type config struct {
		applied   []any
		execErrAt int // number of the Exec call that fails, from 1
		queryErr  error
		scanErr   error
		iterErr   error
	}

	var (
		ctx     context.Context
		conn    *connectionMock
		rows    *RowsMock
		fsys    fstest.MapFS
		getLogs func() string
	)

	setup := func(cfg config) {
		ctx = context.Background()
		getLogs = logger.InitTestLogger()
		applied := make([][]any, 0, len(cfg.applied))
		for _, version := range cfg.applied {
			applied = append(applied, []any{version})
		}
		rows = newRowsMock(applied, cfg.scanErr, cfg.iterErr)
		conn = &connectionMock{
			ExecFunc: func(ctx context.Context, sql string, args ...any) error {
				if cfg.execErrAt > 0 && len(conn.ExecCalls()) == cfg.execErrAt {
					return errors.New("syntax error at or near \"TABLE\"")
				}
				return nil
			},
			QueryFunc: func(ctx context.Context, sql string, args ...any) (postgres.Rows, error) {
				if cfg.queryErr != nil {
					return nil, cfg.queryErr
				}
				return rows, nil
			},
		}
		fsys = fstest.MapFS{
			"0002_more.sql": {Data: []byte("CREATE INDEX b_idx ON b (id)")},
			"0001_init.sql": {Data: []byte("CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);\n")},
		}
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(config{})

		err := migrate(ctx, conn, fsys)

		require.NoError(t, err)
		calls := conn.ExecCalls()
		require.Len(t, calls, 3)
		require.Equal(t, strings.TrimSpace(createMigrationsTableQuery), calls[0].SQL)
		require.Equal(t, "CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);\n;\nINSERT INTO schema_migrations (version) VALUES ('0001_init')", calls[1].SQL)
		require.Equal(t, "CREATE INDEX b_idx ON b (id)\n;\nINSERT INTO schema_migrations (version) VALUES ('0002_more')", calls[2].SQL)
		for _, call := range calls {
			require.Empty(t, call.Args)
		}
		require.Equal(t, strings.TrimSpace(selectMigrationsQuery), conn.QueryCalls()[0].SQL)
		require.Len(t, rows.CloseCalls(), 1)
		require.Contains(t, getLogs(), "INFO: Journal migration applied: version=0001_init\nINFO: Journal migration applied: version=0002_more")
	})

	t.Run("must_pass_skips_applied", func(t *testing.T) {
		setup(config{applied: []any{"0001_init"}})

		err := migrate(ctx, conn, fsys)

		require.NoError(t, err)
		calls := conn.ExecCalls()
		require.Len(t, calls, 2)
		require.True(t, strings.HasPrefix(calls[1].SQL, "CREATE INDEX b_idx"))
		require.NotContains(t, getLogs(), "version=0001_init")
	})

	t.Run("must_pass_nothing_to_apply", func(t *testing.T) {
		setup(config{applied: []any{"0001_init", "0002_more"}})

		err := migrate(ctx, conn, fsys)

		require.NoError(t, err)
		require.Len(t, conn.ExecCalls(), 1)
	})

	t.Run("must_pass_embedded_migrations", func(t *testing.T) {
		setup(config{})

		err := migrate(ctx, conn, migrations.FS)

		require.NoError(t, err)
		calls := conn.ExecCalls()
		require.Len(t, calls, 2)
		for _, table := range []string{"device", "session", "request", "stage_timing", "client_metric", "config_snapshot"} {
			require.Contains(t, calls[1].SQL, "CREATE TABLE "+table+" (")
		}
		require.True(t, strings.HasSuffix(calls[1].SQL, "INSERT INTO schema_migrations (version) VALUES ('0001_init')"))
	})

	t.Run("must_fail_create_table", func(t *testing.T) {
		setup(config{execErrAt: 1})

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, `failed to create schema_migrations: syntax error at or near "TABLE"`)
		require.Empty(t, conn.QueryCalls())
	})

	t.Run("must_fail_query", func(t *testing.T) {
		setup(config{queryErr: errors.New("db down")})

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, "failed to query schema_migrations: db down")
		require.Len(t, conn.ExecCalls(), 1)
	})

	t.Run("must_fail_scan", func(t *testing.T) {
		setup(config{applied: []any{"0001_init"}, scanErr: errors.New("cannot scan")})

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, "failed to scan migration version: cannot scan")
		require.Len(t, rows.CloseCalls(), 1)
	})

	t.Run("must_fail_iterate", func(t *testing.T) {
		setup(config{iterErr: errors.New("conn closed")})

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, "failed to iterate migration versions: conn closed")
	})

	t.Run("must_fail_bad_name", func(t *testing.T) {
		setup(config{})
		fsys["init.sql"] = &fstest.MapFile{Data: []byte("SELECT 1")}

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, `bad migration file name "init.sql": want NNNN_<name>.sql`)
		require.Len(t, conn.ExecCalls(), 1)
	})

	t.Run("must_fail_apply", func(t *testing.T) {
		setup(config{execErrAt: 3})

		err := migrate(ctx, conn, fsys)

		require.EqualError(t, err, `failed to apply migration (version=0002_more): syntax error at or near "TABLE"`)
		require.Contains(t, getLogs(), "version=0001_init")
		require.NotContains(t, getLogs(), "version=0002_more")
	})
}
