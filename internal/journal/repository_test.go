package journal

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/postgres"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// bogusRecord is a record type the repository does not know.
type bogusRecord struct{}

func (*bogusRecord) isRecord() {}

func TestRepository_Write(t *testing.T) {
	var (
		ctx        context.Context
		conn       *connectionMock
		repository *Repository
		startedAt  time.Time
	)

	setup := func(execErr error) {
		ctx = context.Background()
		conn = &connectionMock{
			ExecBatchFunc: func(ctx context.Context, batch *pgx.Batch) error {
				return execErr
			},
		}
		repository = NewRepository(conn)
		startedAt = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	}

	queued := func() []*pgx.QueuedQuery {
		require.Len(t, conn.ExecBatchCalls(), 1)
		return conn.ExecBatchCalls()[0].Batch.QueuedQueries
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(nil)
		objects := json.RawMessage(`[{"label":"chair","label_ru":"стул","direction":"left","distance":"near","hazard":false}]`)
		config := json.RawMessage(`{"placement":"B"}`)
		records := []Record{
			&Session{SessionID: "s-1", DeviceID: "d-1", AppVersion: "0.1.0", Network: "wifi", Label: "P07", StartedAt: startedAt},
			&Request{
				RequestID: "r-1", SessionID: "s-1", Kind: "describe", DescriberMode: "llm", Intent: "describe",
				Status: "degraded", ErrorCode: "", DegradedReason: "llm_timeout", ImageBytes: 48213,
				Transcript: "", AnswerText: "Слева стул.", Hazard: true, Objects: objects, ConfigID: "c9f2a1d0e3b4",
				LLMModel: "fake/fake-llm", LLMSystemFingerprint: "fp-1", LLMError: "timeout_ttft", CreatedAt: startedAt,
				Timings: map[string]float64{"server_total_ms": 1630.4, "detect_ms": 41.3},
			},
			&ClientMetrics{RequestID: "r-1", Metrics: map[string]float64{"trigger_to_first_audio_ms": 1240, "capture_ms": 34}},
			&ConfigSnapshot{ConfigID: "c9f2a1d0e3b4", Config: config, CollectedAt: startedAt},
			&SessionEnd{SessionID: "s-1", EndedAt: startedAt.Add(time.Minute)},
		}

		err := repository.Write(ctx, records)

		require.NoError(t, err)
		queries := queued()
		require.Len(t, queries, 10)
		expected := []struct {
			sql  string
			args []any
		}{
			{upsertDeviceQuery, []any{"d-1", startedAt}},
			{insertSessionQuery, []any{"s-1", "d-1", "0.1.0", "wifi", "P07", startedAt}},
			{insertRequestQuery, []any{
				"r-1", "s-1", "describe", "llm", "describe", "degraded", "", "llm_timeout", 48213, "",
				"Слева стул.", true, objects, "c9f2a1d0e3b4", "fake/fake-llm", "fp-1", "timeout_ttft", startedAt,
			}},
			{insertStageTimingQuery, []any{"r-1", "detect_ms", 41.3}},
			{insertStageTimingQuery, []any{"r-1", "server_total_ms", 1630.4}},
			{insertClientMetricQuery, []any{"r-1", "capture_ms", 34.0}},
			{insertClientMetricQuery, []any{"r-1", "trigger_to_first_audio_ms", 1240.0}},
			{insertConfigSnapshotQuery, []any{"c9f2a1d0e3b4", config, startedAt}},
			{endSessionQuery, []any{"s-1", startedAt.Add(time.Minute)}},
			{touchDeviceQuery, []any{"s-1", startedAt.Add(time.Minute)}},
		}
		for i, query := range queries {
			require.Equal(t, strings.TrimSpace(expected[i].sql), query.SQL)
			require.Equal(t, expected[i].args, query.Arguments)
		}
	})

	t.Run("must_pass_empty_json", func(t *testing.T) {
		setup(nil)

		err := repository.Write(ctx, []Record{
			&Request{RequestID: "r-1", Kind: "passive", Status: "clear"},
			&ConfigSnapshot{ConfigID: "c9f2a1d0e3b4"},
		})

		require.NoError(t, err)
		queries := queued()
		require.Len(t, queries, 2)
		require.Equal(t, json.RawMessage("[]"), queries[0].Arguments[12])
		require.Equal(t, json.RawMessage("{}"), queries[1].Arguments[1])
	})

	t.Run("must_fail_exec_batch", func(t *testing.T) {
		setup(errors.New("db down"))

		err := repository.Write(ctx, []Record{&SessionEnd{SessionID: "s-1", EndedAt: startedAt}})

		require.EqualError(t, err, "failed to write journal records (count=1): db down")
		require.Len(t, conn.ExecBatchCalls(), 1)
	})

	t.Run("must_fail_unknown_record", func(t *testing.T) {
		setup(nil)

		err := repository.Write(ctx, []Record{&bogusRecord{}})

		require.EqualError(t, err, "unknown journal record *journal.bogusRecord")
		require.Empty(t, conn.ExecBatchCalls())
	})
}

func TestRepository_SelectSample(t *testing.T) {
	type config struct {
		queryErrAt int // number of the query that fails, from 1
		scanErrAt  int
		iterErrAt  int
	}

	var (
		ctx        context.Context
		conn       *connectionMock
		rows       []*RowsMock
		repository *Repository
		filter     *stats.Filter
		since      time.Time
		until      time.Time
	)

	results := [][][]any{
		{
			{"describe", "llm", "describe", "ok", "", "", "fake/fake-llm"},
			{"ask", "vlm", "question", "degraded", "llm_unavailable", "http_429", ""},
		},
		{
			{"detect_ms", 41.3},
			{"llm_ttft_ms", 583.0},
		},
		{
			{"describe", 1240.0},
		},
	}

	setup := func(cfg config) {
		ctx = context.Background()
		rows = nil
		conn = &connectionMock{
			QueryFunc: func(ctx context.Context, sql string, args ...any) (postgres.Rows, error) {
				number := len(rows) + 1
				if number == cfg.queryErrAt {
					return nil, errors.New("db down")
				}
				var scanErr, iterErr error
				if number == cfg.scanErrAt {
					scanErr = errors.New("cannot scan NULL into *string")
				}
				if number == cfg.iterErrAt {
					iterErr = errors.New("conn closed")
				}
				mock := newRowsMock(results[number-1], scanErr, iterErr)
				rows = append(rows, mock)
				return mock, nil
			},
		}
		repository = NewRepository(conn)
		filter = &stats.Filter{}
		since = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		until = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(config{})

		sample, err := repository.SelectSample(ctx, filter)

		require.NoError(t, err)
		require.Equal(t, &stats.Sample{
			Requests: []stats.Request{
				{Kind: "describe", DescriberMode: "llm", Intent: "describe", Status: "ok", LLMModel: "fake/fake-llm"},
				{Kind: "ask", DescriberMode: "vlm", Intent: "question", Status: "degraded", DegradedReason: "llm_unavailable", LLMError: "http_429"},
			},
			Stages: []stats.Measurement{{Name: "detect_ms", Value: 41.3}, {Name: "llm_ttft_ms", Value: 583}},
			E2E:    []stats.Measurement{{Name: "describe", Value: 1240}},
		}, sample)
		calls := conn.QueryCalls()
		require.Len(t, calls, 3)
		require.Equal(t, strings.TrimSpace(selectRequestsQuery), calls[0].SQL)
		require.Empty(t, calls[0].Args)
		require.Equal(t, strings.TrimSpace(selectStagesQuery), calls[1].SQL)
		require.Empty(t, calls[1].Args)
		require.Equal(t, strings.TrimSpace(selectE2EQuery), calls[2].SQL)
		require.Equal(t, []any{stats.MetricE2E}, calls[2].Args)
		for _, mock := range rows {
			require.Len(t, mock.CloseCalls(), 1)
		}
	})

	t.Run("must_pass_filters", func(t *testing.T) {
		setup(config{})
		filter = &stats.Filter{Since: &since, Until: &until, Kind: "describe", ConfigID: "c9f2a1d0e3b4"}

		_, err := repository.SelectSample(ctx, filter)

		require.NoError(t, err)
		calls := conn.QueryCalls()
		clause := " AND r.created_at >= $1 AND r.created_at < $2 AND r.kind = $3 AND r.config_id = $4"
		require.Equal(t, strings.TrimSpace(selectRequestsQuery)+clause, calls[0].SQL)
		require.Equal(t, []any{since, until, "describe", "c9f2a1d0e3b4"}, calls[0].Args)
		require.Equal(t, strings.TrimSpace(selectStagesQuery)+clause, calls[1].SQL)
		require.Equal(t, []any{since, until, "describe", "c9f2a1d0e3b4"}, calls[1].Args)
		require.Equal(t, strings.TrimSpace(selectE2EQuery)+
			" AND r.created_at >= $2 AND r.created_at < $3 AND r.kind = $4 AND r.config_id = $5", calls[2].SQL)
		require.Equal(t, []any{stats.MetricE2E, since, until, "describe", "c9f2a1d0e3b4"}, calls[2].Args)
	})

	t.Run("must_pass_partial_filters", func(t *testing.T) {
		setup(config{})
		filter = &stats.Filter{Until: &until, ConfigID: "c9f2a1d0e3b4"}

		_, err := repository.SelectSample(ctx, filter)

		require.NoError(t, err)
		calls := conn.QueryCalls()
		require.Equal(t, strings.TrimSpace(selectRequestsQuery)+" AND r.created_at < $1 AND r.config_id = $2", calls[0].SQL)
		require.Equal(t, []any{until, "c9f2a1d0e3b4"}, calls[0].Args)
	})

	tests := []struct {
		name     string
		cfg      config
		expected string
	}{
		{name: "query_requests", cfg: config{queryErrAt: 1}, expected: "failed to select requests: db down"},
		{name: "scan_request", cfg: config{scanErrAt: 1}, expected: "failed to scan request: cannot scan NULL into *string"},
		{name: "iterate_requests", cfg: config{iterErrAt: 1}, expected: "failed to iterate requests: conn closed"},
		{name: "query_stages", cfg: config{queryErrAt: 2}, expected: "failed to select stage timings: db down"},
		{name: "scan_stage", cfg: config{scanErrAt: 2}, expected: "failed to select stage timings: failed to scan measurement: cannot scan NULL into *string"},
		{name: "iterate_stages", cfg: config{iterErrAt: 2}, expected: "failed to select stage timings: failed to iterate measurements: conn closed"},
		{name: "query_client_metrics", cfg: config{queryErrAt: 3}, expected: "failed to select client metrics: db down"},
		{name: "iterate_client_metrics", cfg: config{iterErrAt: 3}, expected: "failed to select client metrics: failed to iterate measurements: conn closed"},
	}

	for _, tt := range tests {
		t.Run("must_fail_"+tt.name, func(t *testing.T) {
			setup(tt.cfg)

			sample, err := repository.SelectSample(ctx, filter)

			require.Nil(t, sample)
			require.EqualError(t, err, tt.expected)
			for _, mock := range rows {
				require.Len(t, mock.CloseCalls(), 1)
			}
		})
	}
}

// newRowsMock returns rows that give the values row by row; Scan sets the destinations in the order of the values.
func newRowsMock(values [][]any, scanErr, iterErr error) *RowsMock {
	index := -1
	return &RowsMock{
		NextFunc: func() bool {
			index++
			return index < len(values)
		},
		ScanFunc: func(dest ...any) error {
			if scanErr != nil {
				return scanErr
			}
			for i, value := range values[index] {
				reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
			}
			return nil
		},
		ErrFunc: func() error {
			return iterErr
		},
		CloseFunc: func() {},
	}
}
