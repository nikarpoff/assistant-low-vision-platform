package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestNewMetrics(t *testing.T) {
	var m *Metrics

	setup := func() {
		m = NewMetrics()
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()
		m.StageDuration.WithLabelValues("detect_ms", "describe").Observe(0.038)
		m.E2EDuration.WithLabelValues("describe", "wifi").Observe(1.31)
		m.Requests.WithLabelValues("describe", "ok").Inc()
		m.UpstreamErrors.WithLabelValues("llm", "insufficient_balance").Inc()
		m.SessionsActive.Inc()
		m.JournalDropped.Add(2)

		families, err := m.Registry().Gather()

		require.NoError(t, err)
		names := map[string]bool{}
		for _, family := range families {
			names[family.GetName()] = true
		}
		require.Equal(t, map[string]bool{
			"alv_stage_duration_seconds": true,
			"alv_e2e_duration_seconds":   true,
			"alv_requests_total":         true,
			"alv_upstream_errors_total":  true,
			"alv_sessions_active":        true,
			"alv_journal_dropped_total":  true,
		}, names)
		require.Equal(t, 1.0, testutil.ToFloat64(m.UpstreamErrors.WithLabelValues("llm", "insufficient_balance")))
		require.Equal(t, 2.0, testutil.ToFloat64(m.JournalDropped))
		require.Equal(t, 1.0, testutil.ToFloat64(m.SessionsActive))
	})

	t.Run("must_pass_separate_registries", func(t *testing.T) {
		setup()
		other := NewMetrics()

		m.Requests.WithLabelValues("ask", "error").Inc()

		require.Equal(t, 1.0, testutil.ToFloat64(m.Requests.WithLabelValues("ask", "error")))
		require.Equal(t, 0.0, testutil.ToFloat64(other.Requests.WithLabelValues("ask", "error")))
	})

	t.Run("must_pass_label_names", func(t *testing.T) {
		setup()
		m.StageDuration.WithLabelValues("llm_ttft_ms", "ask").Observe(0.4)
		m.E2EDuration.WithLabelValues("passive", "lte").Observe(0.9)
		m.Requests.WithLabelValues("passive", "clear").Inc()
		m.UpstreamErrors.WithLabelValues("tts", "timeout").Inc()

		count, err := testutil.GatherAndCount(m.Registry())

		require.NoError(t, err)
		require.Equal(t, 6, count)
		require.NoError(t, testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP alv_requests_total Requests by kind and final status.
# TYPE alv_requests_total counter
alv_requests_total{kind="passive",status="clear"} 1
# HELP alv_upstream_errors_total Failed calls to the model services and the language model provider by reason.
# TYPE alv_upstream_errors_total counter
alv_upstream_errors_total{reason="timeout",upstream="tts"} 1
`), RequestsName, UpstreamErrorsName))
		require.Equal(t, 1, testutil.CollectAndCount(m.StageDuration, StageDurationName))
		require.Equal(t, 1, testutil.CollectAndCount(m.E2EDuration, E2EDurationName))
	})

	t.Run("must_pass_handler", func(t *testing.T) {
		setup()
		m.JournalDropped.Inc()
		response := httptest.NewRecorder()

		m.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		require.Equal(t, http.StatusOK, response.Code)
		require.Contains(t, response.Body.String(), "alv_journal_dropped_total 1")
		require.Contains(t, response.Body.String(), "alv_sessions_active 0")
		require.NotContains(t, response.Body.String(), "go_goroutines")
	})
}
