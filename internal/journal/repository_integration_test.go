//go:build integration

package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/postgres"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// testDSNVariable names the database of the integration tests; scripts/test-integration.sh sets it.
const testDSNVariable = "APP_TEST_POSTGRES_DSN"

// TestRepository runs the repository and the writer against a real PostgreSQL (AC-34).
// Every case starts from an empty public schema.
func TestRepository(t *testing.T) {
	var (
		ctx        context.Context
		conn       *postgres.Conn
		repository *Repository
		dropped    prometheus.Counter
		start      time.Time
	)

	dsn := os.Getenv(testDSNVariable)
	require.NotEmpty(t, dsn, testDSNVariable+" is empty: run scripts/test-integration.sh")

	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	setup := func() {
		logger.InitTestLogger()
		ctx = context.Background()
		if conn != nil {
			conn.Close()
		}

		var err error
		conn, err = postgres.NewPostgresConnection(&env.Vars{AppPostgresDSN: dsn})
		require.NoError(t, err)
		require.NoError(t, conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"))

		repository = NewRepository(conn)
		dropped = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_journal_dropped_total"})
		start = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	}

	// selectOne reads the only row of the query.
	selectOne := func(t *testing.T, sql string, dest ...any) {
		rows, err := conn.Query(ctx, sql)
		require.NoError(t, err)
		defer rows.Close()

		require.True(t, rows.Next(), sql)
		require.NoError(t, rows.Scan(dest...))
		require.False(t, rows.Next(), sql)
		require.NoError(t, rows.Err())
	}

	count := func(t *testing.T, table string) int64 {
		var n int64
		selectOne(t, "SELECT count(*) FROM "+table, &n)
		return n
	}

	request := func(id int, kind string, configID string, createdAt time.Time) *Request {
		return &Request{
			RequestID:     fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
			SessionID:     "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b",
			Kind:          kind,
			DescriberMode: "llm",
			Intent:        "describe",
			Status:        "ok",
			ConfigID:      configID,
			LLMModel:      "fake/fake-llm",
			CreatedAt:     createdAt,
			Timings:       map[string]float64{"detect_ms": float64(id)},
		}
	}

	t.Run("must_pass_migrations", func(t *testing.T) {
		setup()

		require.NoError(t, repository.Migrate(ctx))
		require.NoError(t, repository.Migrate(ctx))

		var versions string
		selectOne(t, "SELECT string_agg(version, ',' ORDER BY version) FROM schema_migrations", &versions)
		require.Equal(t, "0001_init", versions)
		var tables string
		selectOne(t, `SELECT string_agg(table_name, ',' ORDER BY table_name) FROM information_schema.tables
			WHERE table_schema = 'public'`, &tables)
		require.Equal(t, "client_metric,config_snapshot,device,request,schema_migrations,session,stage_timing", tables)
	})

	t.Run("must_pass_write_and_read", func(t *testing.T) {
		setup()
		require.NoError(t, repository.Migrate(ctx))
		objects := `[{"label": "chair", "label_ru": "стул", "direction": "left", "distance": "near", "hazard": false}]`
		config := `{"placement": "B", "describer_mode": "llm"}`
		records := []Record{
			&Session{SessionID: "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b", DeviceID: "5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10",
				AppVersion: "0.1.0", Network: "wifi", Label: "P07", StartedAt: start},
			&ConfigSnapshot{ConfigID: "c9f2a1d0e3b4", Config: json.RawMessage(config), CollectedAt: start},
			&Request{
				RequestID: "123e4567-e89b-12d3-a456-426614174000", SessionID: "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b",
				Kind: "describe", DescriberMode: "llm", Intent: "describe", Status: "degraded",
				DegradedReason: "llm_unavailable", ImageBytes: 48213, AnswerText: "Слева стул.", Hazard: true,
				Objects: json.RawMessage(objects), ConfigID: "c9f2a1d0e3b4", LLMModel: "qwen/qwen3-8b",
				LLMSystemFingerprint: "fp_7a1", LLMError: "http_429", CreatedAt: start.Add(time.Second),
				Timings: map[string]float64{"detect_ms": 41.3, "server_total_ms": 1630.4},
			},
			&Request{
				RequestID: "0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b", SessionID: "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b",
				Kind: "ask", DescriberMode: "llm", Status: "error", ErrorCode: "asr_empty", Transcript: "",
				ConfigID: "c9f2a1d0e3b4", CreatedAt: start.Add(2 * time.Second),
			},
			&ClientMetrics{RequestID: "123e4567-e89b-12d3-a456-426614174000",
				Metrics: map[string]float64{"trigger_to_first_audio_ms": 1240, "playback_underruns": 0}},
			&SessionEnd{SessionID: "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b", EndedAt: start.Add(time.Minute)},
		}

		require.NoError(t, repository.Write(ctx, records))

		var firstSeen, lastSeen time.Time
		selectOne(t, "SELECT first_seen_at, last_seen_at FROM device WHERE device_id = '5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10'",
			&firstSeen, &lastSeen)
		require.Equal(t, start, firstSeen.UTC())
		require.Equal(t, start.Add(time.Minute), lastSeen.UTC())

		var deviceID, appVersion, network, label string
		var startedAt, endedAt time.Time
		selectOne(t, `SELECT device_id, app_version, network, label, started_at, ended_at FROM session
			WHERE session_id = '8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b'`,
			&deviceID, &appVersion, &network, &label, &startedAt, &endedAt)
		require.Equal(t, []string{"5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10", "0.1.0", "wifi", "P07"},
			[]string{deviceID, appVersion, network, label})
		require.Equal(t, start, startedAt.UTC())
		require.Equal(t, start.Add(time.Minute), endedAt.UTC())

		var (
			sessionID, kind, describerMode, intent, status, errorCode, degradedReason string
			transcript, answerText, storedObjects, configID, llmModel, fingerprint    string
			llmError                                                                  string
			imageBytes                                                                int32
			hazard                                                                    bool
			createdAt                                                                 time.Time
		)
		selectOne(t, `SELECT session_id::text, kind, describer_mode, intent, status, error_code, degraded_reason,
				image_bytes, transcript, answer_text, hazard, objects::text, config_id, llm_model,
				llm_system_fingerprint, llm_error, created_at
			FROM request WHERE request_id = '123e4567-e89b-12d3-a456-426614174000'`,
			&sessionID, &kind, &describerMode, &intent, &status, &errorCode, &degradedReason, &imageBytes,
			&transcript, &answerText, &hazard, &storedObjects, &configID, &llmModel, &fingerprint, &llmError, &createdAt)
		require.Equal(t, "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b", sessionID)
		require.Equal(t, []string{"describe", "llm", "describe", "degraded", "", "llm_unavailable"},
			[]string{kind, describerMode, intent, status, errorCode, degradedReason})
		require.Equal(t, int32(48213), imageBytes)
		require.Equal(t, []string{"", "Слева стул.", "c9f2a1d0e3b4", "qwen/qwen3-8b", "fp_7a1", "http_429"},
			[]string{transcript, answerText, configID, llmModel, fingerprint, llmError})
		require.True(t, hazard)
		require.JSONEq(t, objects, storedObjects)
		require.Equal(t, start.Add(time.Second), createdAt.UTC())

		selectOne(t, `SELECT status, error_code, objects::text FROM request
			WHERE request_id = '0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b'`, &status, &errorCode, &storedObjects)
		require.Equal(t, []string{"error", "asr_empty", "[]"}, []string{status, errorCode, storedObjects})

		var timings, metrics string
		selectOne(t, `SELECT string_agg(stage || '=' || duration_ms, ',' ORDER BY stage) FROM stage_timing
			WHERE request_id = '123e4567-e89b-12d3-a456-426614174000'`, &timings)
		require.Equal(t, "detect_ms=41.3,server_total_ms=1630.4", timings)
		selectOne(t, `SELECT string_agg(name || '=' || value, ',' ORDER BY name) FROM client_metric
			WHERE request_id = '123e4567-e89b-12d3-a456-426614174000'`, &metrics)
		require.Equal(t, "playback_underruns=0,trigger_to_first_audio_ms=1240", metrics)

		var storedConfig string
		var collectedAt time.Time
		selectOne(t, "SELECT config::text, collected_at FROM config_snapshot WHERE config_id = 'c9f2a1d0e3b4'",
			&storedConfig, &collectedAt)
		require.JSONEq(t, config, storedConfig)
		require.Equal(t, start, collectedAt.UTC())

		sample, err := repository.SelectSample(ctx, &stats.Filter{})
		require.NoError(t, err)
		require.ElementsMatch(t, []stats.Request{
			{Kind: "describe", DescriberMode: "llm", Intent: "describe", Status: "degraded",
				DegradedReason: "llm_unavailable", LLMError: "http_429", LLMModel: "qwen/qwen3-8b"},
			{Kind: "ask", DescriberMode: "llm", Status: "error"},
		}, sample.Requests)
		require.ElementsMatch(t, []stats.Measurement{{Name: "detect_ms", Value: 41.3}, {Name: "server_total_ms", Value: 1630.4}},
			sample.Stages)
		require.Equal(t, []stats.Measurement{{Name: "describe", Value: 1240}}, sample.E2E)
	})

	t.Run("must_pass_session_label", func(t *testing.T) {
		setup()
		require.NoError(t, repository.Migrate(ctx))

		require.NoError(t, repository.Write(ctx, []Record{
			&Session{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: "device-1", AppVersion: "0.1.0",
				Network: "cellular", Label: "участник-03", StartedAt: start},
			&Session{SessionID: "22222222-2222-4222-8222-222222222222", DeviceID: "device-1", AppVersion: "0.1.0",
				Network: "wifi", StartedAt: start.Add(time.Hour)},
		}))

		var labels string
		selectOne(t, "SELECT string_agg(session_id::text || ':' || label, ',' ORDER BY started_at) FROM session", &labels)
		require.Equal(t, "11111111-1111-4111-8111-111111111111:участник-03,22222222-2222-4222-8222-222222222222:", labels)
		var firstSeen, lastSeen time.Time
		selectOne(t, "SELECT first_seen_at, last_seen_at FROM device WHERE device_id = 'device-1'", &firstSeen, &lastSeen)
		require.Equal(t, start, firstSeen.UTC())
		require.Equal(t, start.Add(time.Hour), lastSeen.UTC())
		require.Equal(t, int64(1), count(t, "device"))
	})

	t.Run("must_pass_filters", func(t *testing.T) {
		setup()
		require.NoError(t, repository.Migrate(ctx))
		records := []Record{
			request(1, "describe", "aaaaaaaaaaaa", start.Add(-time.Second)), // before since
			request(2, "describe", "aaaaaaaaaaaa", start),                   // since is inclusive
			request(3, "ask", "aaaaaaaaaaaa", start.Add(time.Minute)),
			request(4, "describe", "bbbbbbbbbbbb", start.Add(time.Minute)),
			request(5, "describe", "aaaaaaaaaaaa", start.Add(time.Hour)), // until is exclusive
		}
		for _, record := range records {
			records = append(records, &ClientMetrics{
				RequestID: record.(*Request).RequestID,
				Metrics:   map[string]float64{stats.MetricE2E: 1000 + record.(*Request).Timings["detect_ms"], "capture_ms": 30},
			})
		}
		require.NoError(t, repository.Write(ctx, records))
		since, until := start, start.Add(time.Hour)

		sample, err := repository.SelectSample(ctx, &stats.Filter{Since: &since, Until: &until, Kind: "describe", ConfigID: "aaaaaaaaaaaa"})
		require.NoError(t, err)
		require.Len(t, sample.Requests, 1)
		require.Equal(t, []stats.Measurement{{Name: "detect_ms", Value: 2}}, sample.Stages)
		require.Equal(t, []stats.Measurement{{Name: "describe", Value: 1002}}, sample.E2E)

		sample, err = repository.SelectSample(ctx, &stats.Filter{Since: &since, Until: &until})
		require.NoError(t, err)
		require.Len(t, sample.Requests, 3)
		require.ElementsMatch(t, []stats.Measurement{
			{Name: "describe", Value: 1002}, {Name: "ask", Value: 1003}, {Name: "describe", Value: 1004},
		}, sample.E2E)

		sample, err = repository.SelectSample(ctx, &stats.Filter{ConfigID: "aaaaaaaaaaaa"})
		require.NoError(t, err)
		require.Len(t, sample.Requests, 4)

		sample, err = repository.SelectSample(ctx, &stats.Filter{Kind: "ask"})
		require.NoError(t, err)
		require.Equal(t, []stats.Measurement{{Name: "detect_ms", Value: 3}}, sample.Stages)
	})

	t.Run("must_pass_duplicates_ignored", func(t *testing.T) {
		setup()
		require.NoError(t, repository.Migrate(ctx))
		first := request(1, "describe", "aaaaaaaaaaaa", start)
		snapshot := &ConfigSnapshot{ConfigID: "aaaaaaaaaaaa", Config: json.RawMessage(`{}`), CollectedAt: start}

		require.NoError(t, repository.Write(ctx, []Record{first, snapshot}))
		snapshot.CollectedAt = start.Add(time.Minute)
		require.NoError(t, repository.Write(ctx, []Record{first, snapshot}))

		require.Equal(t, int64(1), count(t, "request"))
		require.Equal(t, int64(1), count(t, "stage_timing"))
		var collectedAt time.Time
		selectOne(t, "SELECT collected_at FROM config_snapshot", &collectedAt)
		require.Equal(t, start, collectedAt.UTC())
	})

	t.Run("must_pass_writer", func(t *testing.T) {
		setup()
		writer := NewWriter(repository, dropped, 256)
		writer.Start()

		for i := 0; i < 150; i++ {
			writer.Write(request(i, "describe", "aaaaaaaaaaaa", start.Add(time.Duration(i)*time.Second)))
		}
		writer.Stop()

		require.Equal(t, StateOK, writer.State())
		require.Equal(t, 0.0, testutil.ToFloat64(dropped))
		require.Equal(t, int64(150), count(t, "request"))
		require.Equal(t, int64(150), count(t, "stage_timing"))
	})

	t.Run("must_pass_writer_rejected_record", func(t *testing.T) {
		setup()
		bad := request(2, "describe", "aaaaaaaaaaaa", start)
		bad.Status = "unknown"
		records := []Record{request(1, "describe", "aaaaaaaaaaaa", start), bad, request(3, "ask", "aaaaaaaaaaaa", start)}
		writer := NewWriter(repository, dropped, 8)
		for _, record := range records {
			writer.Write(record)
		}

		writer.Start()
		writer.Stop()

		require.Equal(t, StateOK, writer.State())
		require.Equal(t, 1.0, testutil.ToFloat64(dropped))
		var ids string
		selectOne(t, "SELECT string_agg(right(request_id::text, 1), ',' ORDER BY request_id) FROM request", &ids)
		require.Equal(t, "1,3", ids)
	})
}
