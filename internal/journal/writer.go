package journal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/postgres"
)

// Writer settings.
const (
	defaultBatchSize       = 100              // records in one transaction
	defaultWriteTimeout    = 5 * time.Second  // one migration or batch, and the final flush on Stop
	defaultWarningInterval = 10 * time.Second // at most one warning of each kind per interval
)

// Texts of the journal warnings and errors, shared by the code and the tests.
const (
	warnJournalDisabled   = "APP_POSTGRES_DSN is empty: the journal is disabled, requests are not recorded"
	warnBufferFullFormat  = "journal buffer is full: records dropped since the previous warning: %d"
	warnWriteFailedFormat = "failed to write journal records: records dropped since the previous warning: %d: %v"
	errBufferFormat       = "APP_JOURNAL_BUFFER must be positive: %d"
)

// repository is the part of Repository the writer uses.
//
//go:generate moq -out repository_moq_test.go . repository
type repository interface {
	Migrate(ctx context.Context) error
	Write(ctx context.Context, records []Record) error
}

// Writer writes the journal asynchronously: Write puts a record in a bounded buffer and returns at
// once, one goroutine takes the records from the buffer and writes them in batches. A record that does
// not fit the buffer or fails to be written is dropped and counted in alv_journal_dropped_total.
type Writer struct {
	repository repository // nil while the journal is disabled
	dropped    prometheus.Counter
	records    chan Record
	stop       chan struct{}
	done       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	started    atomic.Bool
	state      atomic.Value // StateOK or StateDown
	migrated   bool         // owned by the writer goroutine

	batchSize        int
	writeTimeout     time.Duration
	now              func() time.Time
	overflowWarnings *warnLimiter
	writeWarnings    *warnLimiter
}

// Open opens the journal by APP_POSTGRES_DSN and APP_JOURNAL_BUFFER: the connection pool, the
// repository over it and the writer. The database is not contacted here: the writer applies the
// migrations when it starts and retries them before the next records, so an unavailable database
// does not stop the gateway (NFR-4).
//
// An empty APP_POSTGRES_DSN disables the journal: a warning is logged, the writer keeps nothing
// and is in the state "disabled", the repository returns stats.ErrJournalDisabled.
func Open(vars *env.Vars, dropped prometheus.Counter) (*Writer, *Repository, error) {
	if vars.AppPostgresDSN == "" {
		logger.Warning(warnJournalDisabled)
		return &Writer{}, &Repository{}, nil
	}

	if vars.AppJournalBuffer < 1 {
		return nil, nil, fmt.Errorf(errBufferFormat, vars.AppJournalBuffer)
	}

	conn, err := postgres.NewPostgresConnection(vars)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open journal: %w", err)
	}

	repository := NewRepository(conn)
	return NewWriter(repository, dropped, vars.AppJournalBuffer), repository, nil
}

// NewWriter creates a writer over the repository with a buffer of the given number of records.
// It is in the state "down" until the migrations are applied.
func NewWriter(repository repository, dropped prometheus.Counter, buffer int) *Writer {
	w := &Writer{
		repository:       repository,
		dropped:          dropped,
		records:          make(chan Record, buffer),
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
		batchSize:        defaultBatchSize,
		writeTimeout:     defaultWriteTimeout,
		now:              time.Now,
		overflowWarnings: &warnLimiter{interval: defaultWarningInterval},
		writeWarnings:    &warnLimiter{interval: defaultWarningInterval},
	}
	w.state.Store(StateDown)
	return w
}

// Start starts the writer goroutine; it applies the migrations first.
func (w *Writer) Start() {
	if w.repository == nil {
		return
	}

	w.startOnce.Do(func() {
		w.started.Store(true)
		go w.run()
	})
}

// Stop stops the writer: the records already in the buffer are written within one write timeout,
// the rest are dropped. Records written after Stop are dropped when the buffer is full.
func (w *Writer) Stop() {
	if w.repository == nil || !w.started.Load() {
		return
	}

	logger.Info("stopping journal writer...")
	w.stopOnce.Do(func() {
		close(w.stop)
	})
	<-w.done
}

// Write puts the record in the buffer without waiting; a full buffer drops the record.
func (w *Writer) Write(record Record) {
	if w.repository == nil || record == nil || reflect.ValueOf(record).IsNil() {
		return
	}

	select {
	case w.records <- record:
	default:
		w.dropped.Inc()
		if ok, dropped := w.overflowWarnings.note(w.now(), 1); ok {
			logger.Warning(warnBufferFullFormat, dropped)
		}
	}
}

// State returns the state of the journal for /ready: "ok" after a successful write or migration,
// "down" before the migrations and after a failed write, "disabled" without APP_POSTGRES_DSN.
func (w *Writer) State() string {
	if w.repository == nil {
		return StateDisabled
	}
	return w.state.Load().(string)
}

// run is the writer goroutine.
func (w *Writer) run() {
	defer close(w.done)

	ctx, cancel := context.WithTimeout(context.Background(), w.writeTimeout)
	if err := w.ensureMigrated(ctx); err != nil {
		w.fail(0, err)
	}
	cancel()

	for {
		select {
		case <-w.stop:
			w.flushBuffered()
			return
		case record := <-w.records:
			w.flush(w.collect(record))
		}
	}
}

// collect makes a batch of the record and the records waiting in the buffer, up to the batch size.
func (w *Writer) collect(first Record) []Record {
	batch := []Record{first}
	for len(batch) < w.batchSize {
		select {
		case record := <-w.records:
			batch = append(batch, record)
		default:
			return batch
		}
	}
	return batch
}

// flush writes one batch within the write timeout.
func (w *Writer) flush(batch []Record) {
	ctx, cancel := context.WithTimeout(context.Background(), w.writeTimeout)
	defer cancel()

	w.write(ctx, batch)
}

// flushBuffered writes the records that are in the buffer now, all within one write timeout.
func (w *Writer) flushBuffered() {
	ctx, cancel := context.WithTimeout(context.Background(), w.writeTimeout)
	defer cancel()

	for buffered := len(w.records); buffered > 0; {
		batch := make([]Record, 0, min(buffered, w.batchSize))
		for len(batch) < cap(batch) {
			batch = append(batch, <-w.records)
		}
		buffered -= len(batch)
		w.write(ctx, batch)
	}
}

// write writes the batch. When the database rejects it (a server error, e.g. a violated constraint),
// the records are written one by one, so only the rejected ones are lost.
func (w *Writer) write(ctx context.Context, batch []Record) {
	if err := w.ensureMigrated(ctx); err != nil {
		w.fail(len(batch), err)
		return
	}

	err := w.repository.Write(ctx, batch)
	if err == nil {
		w.state.Store(StateOK)
		return
	}

	var pgErr *pgconn.PgError
	if len(batch) == 1 || !errors.As(err, &pgErr) {
		w.fail(len(batch), err)
		return
	}

	for _, record := range batch {
		if err := w.repository.Write(ctx, []Record{record}); err != nil {
			w.fail(1, err)
		}
	}
}

// ensureMigrated applies the migrations once.
func (w *Writer) ensureMigrated(ctx context.Context) error {
	if w.migrated {
		return nil
	}

	if err := w.repository.Migrate(ctx); err != nil {
		return fmt.Errorf("failed to migrate journal database: %w", err)
	}

	w.migrated = true
	w.state.Store(StateOK)
	return nil
}

// fail counts the dropped records, updates the state and warns at most once per interval.
// A server error after the migrations means the database works and only rejected the records.
func (w *Writer) fail(count int, err error) {
	w.dropped.Add(float64(count))

	var pgErr *pgconn.PgError
	if w.migrated && errors.As(err, &pgErr) {
		w.state.Store(StateOK)
	} else {
		w.state.Store(StateDown)
	}

	if ok, dropped := w.writeWarnings.note(w.now(), count); ok {
		logger.Warning(warnWriteFailedFormat, dropped, err)
	}
}

// warnLimiter lets a warning through at most once per interval and counts the records dropped in between.
type warnLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	dropped  int
}

// note counts the dropped records and reports whether a warning is due; then it also returns the
// number of records dropped since the previous warning.
func (l *warnLimiter) note(now time.Time, dropped int) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.dropped += dropped
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		return false, 0
	}

	l.last = now
	total := l.dropped
	l.dropped = 0
	return true, total
}
