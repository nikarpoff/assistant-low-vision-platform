package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/postgres"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
	"github.com/nikarpoff/assistant-low-vision-platform/migrations"
)

// connection is the part of postgres.Conn the repository uses.
//
//go:generate moq -out connection_moq_test.go . connection
type connection interface {
	Exec(ctx context.Context, sql string, args ...any) error
	ExecBatch(ctx context.Context, batch *pgx.Batch) error
	Query(ctx context.Context, sql string, args ...any) (postgres.Rows, error)
	Close()
}

//go:generate moq -pkg journal -out rows_moq_test.go ../postgres Rows

const upsertDeviceQuery = `
	INSERT INTO device (device_id, first_seen_at, last_seen_at)
	VALUES ($1, $2, $2)
	ON CONFLICT (device_id) DO UPDATE SET last_seen_at = GREATEST(device.last_seen_at, EXCLUDED.last_seen_at)
`

const insertSessionQuery = `
	INSERT INTO session (session_id, device_id, app_version, network, label, started_at)
	VALUES ($1, $2, $3, $4, $5, $6)
	ON CONFLICT (session_id) DO NOTHING
`

const endSessionQuery = `
	UPDATE session SET ended_at = $2
	WHERE session_id = $1
`

const touchDeviceQuery = `
	UPDATE device SET last_seen_at = GREATEST(device.last_seen_at, $2)
	FROM session
	WHERE session.session_id = $1 AND device.device_id = session.device_id
`

const insertRequestQuery = `
	INSERT INTO request (
		request_id, session_id, kind, describer_mode, intent, status, error_code, degraded_reason,
		image_bytes, transcript, answer_text, hazard, objects, config_id,
		llm_model, llm_system_fingerprint, llm_error, created_at
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	ON CONFLICT (request_id) DO NOTHING
`

const insertStageTimingQuery = `
	INSERT INTO stage_timing (request_id, stage, duration_ms)
	VALUES ($1, $2, $3)
	ON CONFLICT (request_id, stage) DO NOTHING
`

const insertClientMetricQuery = `
	INSERT INTO client_metric (request_id, name, value)
	VALUES ($1, $2, $3)
	ON CONFLICT (request_id, name) DO NOTHING
`

const insertConfigSnapshotQuery = `
	INSERT INTO config_snapshot (config_id, config, collected_at)
	VALUES ($1, $2, $3)
	ON CONFLICT (config_id) DO NOTHING
`

const selectRequestsQuery = `
	SELECT r.kind, r.describer_mode, r.intent, r.status, r.degraded_reason, r.llm_error, r.llm_model
	FROM request r
	WHERE 1=1
`

const selectStagesQuery = `
	SELECT t.stage, t.duration_ms
	FROM stage_timing t
	JOIN request r ON r.request_id = t.request_id
	WHERE 1=1
`

// selectE2EQuery takes the name of the E2E metric as $1.
const selectE2EQuery = `
	SELECT r.kind, m.value
	FROM client_metric m
	JOIN request r ON r.request_id = m.request_id
	WHERE m.name = $1
`

// errUnknownRecordFormat is the error for a record of an unknown type.
const errUnknownRecordFormat = "unknown journal record %T"

// emptyObjects and emptyConfig replace an empty JSON value.
var (
	emptyObjects = json.RawMessage("[]")
	emptyConfig  = json.RawMessage("{}")
)

// Repository writes the journal records to PostgreSQL and selects the data of /v1/stats.
type Repository struct {
	conn connection // nil while the journal is disabled
}

// NewRepository creates a new Repository over the connection.
func NewRepository(conn connection) *Repository {
	return &Repository{conn: conn}
}

// Migrate applies the migrations of the repository (migrations/) that the database does not have yet.
func (r *Repository) Migrate(ctx context.Context) error {
	if r.conn == nil {
		return stats.ErrJournalDisabled
	}
	return migrate(ctx, r.conn, migrations.FS)
}

// Write writes the records in one transaction: either all of them or none.
func (r *Repository) Write(ctx context.Context, records []Record) error {
	if r.conn == nil {
		return stats.ErrJournalDisabled
	}

	batch := &pgx.Batch{}
	for _, record := range records {
		if err := queueRecord(batch, record); err != nil {
			return err
		}
	}

	if err := r.conn.ExecBatch(ctx, batch); err != nil {
		return fmt.Errorf("failed to write journal records (count=%d): %w", len(records), err)
	}

	return nil
}

// SelectSample selects the requests matching the filter, the durations of their stages and their E2E
// (trigger_to_first_audio_ms of client.metrics). While the journal is disabled it returns stats.ErrJournalDisabled.
func (r *Repository) SelectSample(ctx context.Context, filter *stats.Filter) (*stats.Sample, error) {
	if r.conn == nil {
		return nil, stats.ErrJournalDisabled
	}

	clause, args := filterClause(filter, nil)
	requests, err := r.selectRequests(ctx, strings.TrimSpace(selectRequestsQuery)+clause, args)
	if err != nil {
		return nil, err
	}

	stages, err := r.selectMeasurements(ctx, strings.TrimSpace(selectStagesQuery)+clause, args)
	if err != nil {
		return nil, fmt.Errorf("failed to select stage timings: %w", err)
	}

	clause, args = filterClause(filter, []any{stats.MetricE2E})
	e2e, err := r.selectMeasurements(ctx, strings.TrimSpace(selectE2EQuery)+clause, args)
	if err != nil {
		return nil, fmt.Errorf("failed to select client metrics: %w", err)
	}

	return &stats.Sample{Requests: requests, Stages: stages, E2E: e2e}, nil
}

// Close closes the connection pool.
func (r *Repository) Close() {
	if r.conn != nil {
		r.conn.Close()
	}
}

// selectRequests selects the request fields of the statistics.
func (r *Repository) selectRequests(ctx context.Context, query string, args []any) ([]stats.Request, error) {
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to select requests: %w", err)
	}
	defer rows.Close()

	requests := []stats.Request{}
	for rows.Next() {
		var request stats.Request
		err := rows.Scan(
			&request.Kind,
			&request.DescriberMode,
			&request.Intent,
			&request.Status,
			&request.DegradedReason,
			&request.LLMError,
			&request.LLMModel,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan request: %w", err)
		}
		requests = append(requests, request)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate requests: %w", err)
	}

	return requests, nil
}

// selectMeasurements selects the rows (name, value).
func (r *Repository) selectMeasurements(ctx context.Context, query string, args []any) ([]stats.Measurement, error) {
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	measurements := []stats.Measurement{}
	for rows.Next() {
		var measurement stats.Measurement
		if err := rows.Scan(&measurement.Name, &measurement.Value); err != nil {
			return nil, fmt.Errorf("failed to scan measurement: %w", err)
		}
		measurements = append(measurements, measurement)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate measurements: %w", err)
	}

	return measurements, nil
}

// filterClause appends the conditions of the filter on the request r to the arguments already taken by the query.
func filterClause(filter *stats.Filter, args []any) (string, []any) {
	var clause strings.Builder
	add := func(condition string, value any) {
		args = append(args, value)
		clause.WriteString(" AND ")
		fmt.Fprintf(&clause, condition, len(args))
	}

	if filter.Since != nil {
		add("r.created_at >= $%d", *filter.Since)
	}
	if filter.Until != nil {
		add("r.created_at < $%d", *filter.Until)
	}
	if filter.Kind != "" {
		add("r.kind = $%d", filter.Kind)
	}
	if filter.ConfigID != "" {
		add("r.config_id = $%d", filter.ConfigID)
	}

	return clause.String(), args
}

// queueRecord queues the statements of the record.
func queueRecord(batch *pgx.Batch, record Record) error {
	switch typed := record.(type) {
	case *Session:
		batch.Queue(strings.TrimSpace(upsertDeviceQuery), typed.DeviceID, typed.StartedAt)
		batch.Queue(strings.TrimSpace(insertSessionQuery),
			typed.SessionID, typed.DeviceID, typed.AppVersion, typed.Network, typed.Label, typed.StartedAt)
	case *SessionEnd:
		batch.Queue(strings.TrimSpace(endSessionQuery), typed.SessionID, typed.EndedAt)
		batch.Queue(strings.TrimSpace(touchDeviceQuery), typed.SessionID, typed.EndedAt)
	case *Request:
		queueRequest(batch, typed)
	case *ClientMetrics:
		for _, name := range sortedKeys(typed.Metrics) {
			batch.Queue(strings.TrimSpace(insertClientMetricQuery), typed.RequestID, name, typed.Metrics[name])
		}
	case *ConfigSnapshot:
		config := typed.Config
		if len(config) == 0 {
			config = emptyConfig
		}
		batch.Queue(strings.TrimSpace(insertConfigSnapshotQuery), typed.ConfigID, config, typed.CollectedAt)
	default:
		return fmt.Errorf(errUnknownRecordFormat, record)
	}

	return nil
}

// queueRequest queues the request and the durations of its stages.
func queueRequest(batch *pgx.Batch, request *Request) {
	objects := request.Objects
	if len(objects) == 0 {
		objects = emptyObjects
	}

	batch.Queue(strings.TrimSpace(insertRequestQuery),
		request.RequestID,
		request.SessionID,
		request.Kind,
		request.DescriberMode,
		request.Intent,
		request.Status,
		request.ErrorCode,
		request.DegradedReason,
		request.ImageBytes,
		request.Transcript,
		request.AnswerText,
		request.Hazard,
		objects,
		request.ConfigID,
		request.LLMModel,
		request.LLMSystemFingerprint,
		request.LLMError,
		request.CreatedAt,
	)

	for _, stage := range sortedKeys(request.Timings) {
		batch.Queue(strings.TrimSpace(insertStageTimingQuery), request.RequestID, stage, request.Timings[stage])
	}
}

// sortedKeys returns the keys of the map in ascending order, so the statements go in a stable order.
func sortedKeys(values map[string]float64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
