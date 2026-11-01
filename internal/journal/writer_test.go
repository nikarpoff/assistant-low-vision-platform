package journal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// errConnectionRefused is what the repository returns while the database is down.
var errConnectionRefused = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

// waitTimeout bounds every wait of the tests, so a blocking Write fails the test instead of hanging it.
const waitTimeout = 5 * time.Second

func TestJournal(t *testing.T) {
	type config struct {
		buffer      int
		blocked     bool // the repository holds every write until release is closed
		unavailable bool // the repository waits for the deadline and fails, as an unreachable database
	}

	var (
		repository *repositoryMock
		dropped    prometheus.Counter
		writer     *Writer
		getLogs    func() string
		clock      atomic.Int64
		release    chan struct{}
		entered    chan struct{}
		available  atomic.Bool
		migrateErr atomic.Pointer[error]
		writtenMu  sync.Mutex
		written    []Record
	)

	setup := func(cfg config) {
		getLogs = logger.InitTestLogger()
		dropped = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_journal_dropped_total"})
		clock.Store(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC).UnixNano())
		release = make(chan struct{})
		if !cfg.blocked {
			close(release)
		}
		entered = make(chan struct{}, 100)
		available.Store(!cfg.unavailable)
		migrateErr.Store(nil)
		written = nil

		repository = &repositoryMock{
			MigrateFunc: func(ctx context.Context) error {
				if !available.Load() {
					<-ctx.Done()
					return errConnectionRefused
				}
				if err := migrateErr.Load(); err != nil {
					return *err
				}
				return nil
			},
			WriteFunc: func(ctx context.Context, records []Record) error {
				entered <- struct{}{}
				<-release
				if !available.Load() {
					<-ctx.Done()
					return errConnectionRefused
				}
				for _, record := range records {
					if request, ok := record.(*Request); ok && request.Kind == "bogus" {
						return &pgconn.PgError{Code: "23514", Message: `new row for relation "request" violates check constraint "request_kind_check"`}
					}
				}
				writtenMu.Lock()
				defer writtenMu.Unlock()
				written = append(written, records...)
				return nil
			},
		}

		writer = NewWriter(repository, dropped, cfg.buffer)
		writer.writeTimeout = 50 * time.Millisecond
		writer.now = func() time.Time {
			return time.Unix(0, clock.Load())
		}
	}

	writtenRecords := func() []Record {
		writtenMu.Lock()
		defer writtenMu.Unlock()
		return append([]Record(nil), written...)
	}

	t.Run("must_pass_async_non_blocking", func(t *testing.T) {
		setup(config{buffer: 16, blocked: true})
		records := newRequests(10)
		writer.Start()

		writer.Write(records[0])
		waitFor(t, entered)
		writesDone := make(chan struct{})
		go func() {
			defer close(writesDone)
			for _, record := range records[1:] {
				writer.Write(record)
			}
		}()
		waitFor(t, writesDone)

		require.Len(t, repository.WriteCalls(), 1)
		close(release)
		writer.Stop()
		require.Equal(t, records, writtenRecords())
		require.Len(t, repository.WriteCalls(), 2)
		require.Len(t, repository.WriteCalls()[1].Records, 9)
		require.Len(t, repository.MigrateCalls(), 1)
		require.Equal(t, 0.0, testutil.ToFloat64(dropped))
		require.Equal(t, StateOK, writer.State())
	})

	t.Run("must_pass_dropped_on_overflow", func(t *testing.T) {
		setup(config{buffer: 2, blocked: true})
		records := newRequests(7)
		writer.Start()

		writer.Write(records[0])
		waitFor(t, entered)
		for _, record := range records[1:6] {
			writer.Write(record)
		}
		droppedBeforeWarning := testutil.ToFloat64(dropped)
		logsBeforeWarning := getLogs()
		clock.Add(int64(defaultWarningInterval))
		writer.Write(records[6])

		require.Equal(t, 3.0, droppedBeforeWarning)
		require.Equal(t, 1, strings.Count(logsBeforeWarning, "WARNING: journal buffer is full"))
		require.Contains(t, logsBeforeWarning, "WARNING: "+fmt.Sprintf(warnBufferFullFormat, 1))
		require.Equal(t, 4.0, testutil.ToFloat64(dropped))
		require.Contains(t, getLogs(), "WARNING: "+fmt.Sprintf(warnBufferFullFormat, 3))
		close(release)
		writer.Stop()
		require.Equal(t, records[:3], writtenRecords())
	})

	t.Run("must_pass_db_unavailable_does_not_block", func(t *testing.T) {
		setup(config{buffer: 32, unavailable: true})
		records := newRequests(20)
		writer.Start()

		writesDone := make(chan struct{})
		go func() {
			defer close(writesDone)
			for _, record := range records {
				writer.Write(record)
			}
		}()
		waitFor(t, writesDone)

		require.Eventually(t, func() bool {
			return testutil.ToFloat64(dropped) == 20
		}, waitTimeout, 5*time.Millisecond)
		require.Equal(t, StateDown, writer.State())
		require.Empty(t, writtenRecords())
		logs := getLogs()
		require.Equal(t, 1, strings.Count(logs, "WARNING: failed to write journal records"))
		require.Contains(t, logs, "WARNING: "+fmt.Sprintf(warnWriteFailedFormat, 0,
			"failed to migrate journal database: "+errConnectionRefused.Error()))
		require.NotContains(t, logs, "journal buffer is full")

		// the database is back
		available.Store(true)
		recovered := newRequests(1)[0]
		writer.Write(recovered)

		require.Eventually(t, func() bool {
			return len(writtenRecords()) == 1
		}, waitTimeout, 5*time.Millisecond)
		require.Equal(t, []Record{recovered}, writtenRecords())
		require.Equal(t, StateOK, writer.State())
		writer.Stop()
		require.Equal(t, 20.0, testutil.ToFloat64(dropped))
	})

	t.Run("must_pass_disabled_without_dsn", func(t *testing.T) {
		setup(config{buffer: 1})

		disabledWriter, disabledRepository, err := Open(&env.Vars{AppPostgresDSN: "", AppJournalBuffer: 1024}, dropped)

		require.NoError(t, err)
		require.Contains(t, getLogs(), "WARNING: "+warnJournalDisabled)
		require.Equal(t, StateDisabled, disabledWriter.State())
		disabledWriter.Start()
		disabledWriter.Write(newRequests(1)[0])
		disabledWriter.Stop()
		require.Equal(t, 0.0, testutil.ToFloat64(dropped))
		sample, err := disabledRepository.SelectSample(context.Background(), &stats.Filter{})
		require.Nil(t, sample)
		require.ErrorIs(t, err, stats.ErrJournalDisabled)
		require.ErrorIs(t, disabledRepository.Write(context.Background(), newRequests(1)), stats.ErrJournalDisabled)
		require.ErrorIs(t, disabledRepository.Migrate(context.Background()), stats.ErrJournalDisabled)
		disabledRepository.Close()
	})

	t.Run("must_pass_migration_retried", func(t *testing.T) {
		setup(config{buffer: 4})
		failure := errors.New(`relation "device" already exists`)
		migrateErr.Store(&failure)
		writer.Start()
		require.Eventually(t, func() bool {
			return strings.Contains(getLogs(), "failed to migrate journal database")
		}, waitTimeout, 5*time.Millisecond)

		migrateErr.Store(nil)
		record := newRequests(1)[0]
		writer.Write(record)
		writer.Stop()

		require.Len(t, repository.MigrateCalls(), 2)
		require.Equal(t, []Record{record}, writtenRecords())
		require.Equal(t, StateOK, writer.State())
		require.Contains(t, getLogs(), "WARNING: "+fmt.Sprintf(warnWriteFailedFormat, 0,
			"failed to migrate journal database: "+failure.Error()))
	})

	t.Run("must_pass_rejected_record_written_one_by_one", func(t *testing.T) {
		setup(config{buffer: 4})
		records := newRequests(3)
		records[1].(*Request).Kind = "bogus"
		for _, record := range records {
			writer.Write(record)
		}

		writer.Start()
		writer.Stop()

		require.Equal(t, []Record{records[0], records[2]}, writtenRecords())
		require.Len(t, repository.WriteCalls(), 4)
		require.Len(t, repository.WriteCalls()[0].Records, 3)
		require.Equal(t, 1.0, testutil.ToFloat64(dropped))
		require.Equal(t, StateOK, writer.State())
		require.Contains(t, getLogs(), `violates check constraint "request_kind_check"`)
	})

	t.Run("must_pass_stop_flushes_buffer", func(t *testing.T) {
		setup(config{buffer: 8})
		records := newRequests(5)
		for _, record := range records {
			writer.Write(record)
		}

		writer.Start()
		writer.Stop()
		writer.Stop()
		writer.Write(newRequests(1)[0])

		require.Equal(t, records, writtenRecords())
		require.Len(t, writer.records, 1)
		require.Equal(t, 0.0, testutil.ToFloat64(dropped))
	})

	t.Run("must_pass_stop_without_start", func(t *testing.T) {
		setup(config{buffer: 1})

		writer.Stop()

		require.Empty(t, repository.MigrateCalls())
		require.Equal(t, StateDown, writer.State())
	})

	t.Run("must_pass_nil_record_ignored", func(t *testing.T) {
		setup(config{buffer: 1})

		writer.Write(nil)
		writer.Write((*Request)(nil))

		require.Empty(t, writer.records)
		require.Equal(t, 0.0, testutil.ToFloat64(dropped))
	})
}

func TestOpen(t *testing.T) {
	var (
		vars    *env.Vars
		dropped prometheus.Counter
	)

	setup := func(dsn string, buffer int) {
		logger.InitTestLogger()
		vars = &env.Vars{AppPostgresDSN: dsn, AppJournalBuffer: buffer}
		dropped = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_journal_dropped_total"})
	}

	t.Run("must_pass", func(t *testing.T) {
		setup("postgres://alv@127.0.0.1:1/alv?sslmode=disable", 8)

		writer, repository, err := Open(vars, dropped)

		require.NoError(t, err)
		defer repository.Close()
		require.Equal(t, StateDown, writer.State())
		require.Equal(t, 8, cap(writer.records))
		require.NotNil(t, repository.conn)
		require.Same(t, repository, writer.repository)
	})

	t.Run("must_fail_bad_buffer", func(t *testing.T) {
		setup("postgres://alv@127.0.0.1:1/alv?sslmode=disable", 0)

		writer, repository, err := Open(vars, dropped)

		require.Nil(t, writer)
		require.Nil(t, repository)
		require.EqualError(t, err, "APP_JOURNAL_BUFFER must be positive: 0")
	})

	t.Run("must_fail_bad_dsn", func(t *testing.T) {
		setup("postgres://alv:secret-password@127.0.0.1:port/alv", 8)

		writer, repository, err := Open(vars, dropped)

		require.Nil(t, writer)
		require.Nil(t, repository)
		require.EqualError(t, err, "failed to open journal: failed to parse APP_POSTGRES_DSN: not a PostgreSQL connection string")
	})
}

func TestWarnLimiter(t *testing.T) {
	var (
		limiter *warnLimiter
		start   time.Time
	)

	setup := func() {
		limiter = &warnLimiter{interval: 10 * time.Second}
		start = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		firstOK, firstDropped := limiter.note(start, 2)
		secondOK, _ := limiter.note(start.Add(9*time.Second), 3)
		thirdOK, _ := limiter.note(start.Add(9999*time.Millisecond), 4)
		fourthOK, fourthDropped := limiter.note(start.Add(10*time.Second), 1)

		require.True(t, firstOK)
		require.Equal(t, 2, firstDropped)
		require.False(t, secondOK)
		require.False(t, thirdOK)
		require.True(t, fourthOK)
		require.Equal(t, 8, fourthDropped)
	})
}

// newRequests returns request records with distinct identifiers.
func newRequests(n int) []Record {
	records := make([]Record, n)
	for i := range records {
		records[i] = &Request{
			RequestID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			SessionID: "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b",
			Kind:      stats.KindDescribe,
			Status:    stats.StatusOK,
		}
	}
	return records
}

// waitFor waits until the channel gives a value or is closed.
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(waitTimeout):
		require.FailNow(t, "timed out waiting")
	}
}
