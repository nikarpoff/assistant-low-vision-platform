package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
)

func TestNewPostgresConnection(t *testing.T) {
	var (
		vars *env.Vars
	)

	setup := func(dsn string) {
		vars = &env.Vars{AppPostgresDSN: dsn}
	}

	t.Run("must_pass", func(t *testing.T) {
		setup("postgres://alv:secret-password@127.0.0.1:1/alv?sslmode=disable")

		conn, err := NewPostgresConnection(vars)

		require.NoError(t, err)
		defer conn.Close()
		config := conn.pool.Config()
		require.Equal(t, int32(maxConns), config.MaxConns)
		require.Equal(t, connectTimeout, config.ConnConfig.ConnectTimeout)
		require.Equal(t, "alv", config.ConnConfig.Database)
	})

	t.Run("must_pass_dsn_connect_timeout", func(t *testing.T) {
		setup("postgres://alv@127.0.0.1:1/alv?sslmode=disable&connect_timeout=2")

		conn, err := NewPostgresConnection(vars)

		require.NoError(t, err)
		defer conn.Close()
		require.Equal(t, 2*time.Second, conn.pool.Config().ConnConfig.ConnectTimeout)
	})

	t.Run("must_fail_bad_dsn", func(t *testing.T) {
		setup("postgres://alv:secret-password@127.0.0.1:port/alv")

		conn, err := NewPostgresConnection(vars)

		require.Nil(t, conn)
		require.EqualError(t, err, errParseDSN)
		require.NotContains(t, err.Error(), "secret-password")
	})
}
