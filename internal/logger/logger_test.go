package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogger(t *testing.T) {
	var getLogs func() string

	setup := func() {
		getLogs = InitTestLogger()
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		Debug("Incoming request: request_id=%s", "r-1")
		Info("Server listening on port %d", 80)
		Warning("journal buffer is full: dropped=%d", 3)
		Error(fmt.Errorf("failed to call detector: %w", errors.New("connection refused")))

		require.Equal(t, "DEBUG: Incoming request: request_id=r-1\n"+
			"INFO: Server listening on port 80\n"+
			"WARNING: journal buffer is full: dropped=3\n"+
			"ERROR: failed to call detector: connection refused\n", getLogs())
	})

	t.Run("must_pass_level_filter", func(t *testing.T) {
		setup()
		SetLevel(LevelWarning)

		Debug("hidden debug")
		Info("hidden info")
		Warning("shown warning")
		Error(errors.New("shown error"))

		require.Equal(t, "WARNING: shown warning\nERROR: shown error\n", getLogs())
	})

	t.Run("must_pass_debug_hidden_at_info", func(t *testing.T) {
		setup()
		SetLevel(LevelInfo)

		Debug("question text=%s", "где дверь")
		Info("Request finished: request_id=%s", "r-2")

		require.Equal(t, "INFO: Request finished: request_id=r-2\n", getLogs())
		require.NotContains(t, getLogs(), "где дверь")
	})

	t.Run("must_pass_stdout_format", func(t *testing.T) {
		setup()
		reader, writer, err := os.Pipe()
		require.NoError(t, err)
		stdout := os.Stdout
		os.Stdout = writer
		InitStdoutLogger()
		os.Stdout = stdout

		Debug("hidden at the default level")
		Info("Starting gateway")
		require.NoError(t, writer.Close())
		written, err := io.ReadAll(reader)
		InitTestLogger()

		require.NoError(t, err)
		require.Regexp(t, `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6} INFO: Starting gateway\n$`, string(written))
	})

	t.Run("must_pass_concurrent_writes", func(t *testing.T) {
		setup()
		var wg sync.WaitGroup

		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				Info("message n=%d", n)
			}(i)
		}
		wg.Wait()

		require.Len(t, strings.Split(strings.TrimSuffix(getLogs(), "\n"), "\n"), 50)
	})

	t.Run("must_pass_parse_level", func(t *testing.T) {
		setup()

		for name, expected := range map[string]Level{
			"DEBUG":     LevelDebug,
			"info":      LevelInfo,
			" Warning ": LevelWarning,
			"ERROR":     LevelError,
		} {
			level, err := ParseLevel(name)

			require.NoError(t, err)
			require.Equal(t, expected, level)
		}
	})

	t.Run("must_fail_unknown_level", func(t *testing.T) {
		setup()

		level, err := ParseLevel("verbose")

		require.Equal(t, LevelInfo, level)
		require.ErrorIs(t, err, ErrUnknownLevel)
		require.EqualError(t, err, fmt.Errorf(unknownLevelFormat, ErrUnknownLevel, "verbose").Error())
	})

	t.Run("must_pass_level_names", func(t *testing.T) {
		setup()

		require.Equal(t, "DEBUG", LevelDebug.String())
		require.Equal(t, "INFO", LevelInfo.String())
		require.Equal(t, "WARNING", LevelWarning.String())
		require.Equal(t, "ERROR", LevelError.String())
		require.Equal(t, "LEVEL(9)", Level(9).String())
	})
}
