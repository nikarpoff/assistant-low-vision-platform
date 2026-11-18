package statshandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/handlers/adminauth"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// TestStats checks GET /v1/stats behind the admin token, as the gateway serves it.
func TestStats(t *testing.T) {
	var (
		service  *StatsServiceMock
		endpoint http.Handler
		request  *http.Request
		response *httptest.ResponseRecorder
		result   *stats.GetStatsResponse
		getLogs  func() string
	)

	setup := func(serviceErr error) {
		getLogs = logger.InitTestLogger()
		share := 0.04
		configID := "c9f2a1d0e3b4"
		result = &stats.GetStatsResponse{
			ConfigID:         &configID,
			Stages:           map[string]stats.Summary{"detect_ms": {N: 300, P50: 38.1, P95: 61, Mean: 40.2}},
			E2E:              map[string]stats.Summary{"describe": {N: 300, P50: 1310, P95: 1820, Mean: 1350.4}},
			Degraded:         map[string]stats.Share{"llm_timeout": {N: 9, Share: 0.03}},
			LLMErrors:        map[string]int{"timeout_ttft": 9},
			LLMFallbackShare: &share,
			LLMModels:        map[string]int{"fake/fake-llm": 312},
		}
		service = &StatsServiceMock{
			GetStatsFunc: func(ctx context.Context, req *stats.GetStatsRequest) (*stats.GetStatsResponse, error) {
				if serviceErr != nil {
					return nil, serviceErr
				}
				return result, nil
			},
		}
		vars := &env.Vars{AppAdminToken: "admin-token"}
		endpoint = adminauth.NewMiddleware(vars).Handle(http.HandlerFunc(NewHandler(service).GetStats))
		request = httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
		request.Header.Set("Authorization", "Bearer admin-token")
		response = httptest.NewRecorder()
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(nil)
		request.URL.RawQuery = "since=2026-11-01T10:00:00Z&until=2026-11-01T12:00:00Z&kind=describe&config_id=c9f2a1d0e3b4"

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		expected, err := json.Marshal(result)
		require.NoError(t, err)
		require.JSONEq(t, string(expected), response.Body.String())
		require.Len(t, service.GetStatsCalls(), 1)
		require.Equal(t, &stats.GetStatsRequest{
			Since:    "2026-11-01T10:00:00Z",
			Until:    "2026-11-01T12:00:00Z",
			Kind:     "describe",
			ConfigID: "c9f2a1d0e3b4",
		}, service.GetStatsCalls()[0].Req)
	})

	t.Run("must_pass_without_filters", func(t *testing.T) {
		setup(nil)

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, &stats.GetStatsRequest{}, service.GetStatsCalls()[0].Req)
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup(nil)
		request.Header.Del("Authorization")

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.JSONEq(t, `{"error": "unauthorized"}`, response.Body.String())
		require.Empty(t, service.GetStatsCalls())
	})

	t.Run("must_fail_wrong_token", func(t *testing.T) {
		setup(nil)
		request.Header.Set("Authorization", "Bearer device-token")

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Empty(t, service.GetStatsCalls())
	})

	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{name: "bad_since", query: "since=2026-11-01+10:00", expected: "validation failed for fields: since"},
		{name: "bad_until", query: "until=tomorrow", expected: "validation failed for fields: until"},
		{name: "unknown_kind", query: "kind=photo", expected: "validation failed for fields: kind"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("must_fail_%s", tt.name), func(t *testing.T) {
			setup(nil)
			request.URL.RawQuery = tt.query

			endpoint.ServeHTTP(response, request)

			require.Equal(t, http.StatusUnprocessableEntity, response.Code)
			require.JSONEq(t, fmt.Sprintf(`{"error": %q}`, tt.expected), response.Body.String())
			require.Empty(t, service.GetStatsCalls())
		})
	}

	t.Run("must_fail_service_validation", func(t *testing.T) {
		setup(fmt.Errorf("%w: %w", stats.ErrValidationFailed, errors.New("until must be later than since")))
		request.URL.RawQuery = "since=2026-11-01T12:00:00Z&until=2026-11-01T10:00:00Z"

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusUnprocessableEntity, response.Code)
		require.JSONEq(t, `{"error": "validation failed: until must be later than since"}`, response.Body.String())
	})

	t.Run("must_fail_journal_disabled", func(t *testing.T) {
		setup(fmt.Errorf("failed to select journal sample: %w", stats.ErrJournalDisabled))

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.JSONEq(t, `{"error": "journal is disabled"}`, response.Body.String())
	})

	t.Run("must_fail_internal_error", func(t *testing.T) {
		setup(errors.New("failed to select journal sample: db down"))

		endpoint.ServeHTTP(response, request)

		require.Equal(t, http.StatusInternalServerError, response.Code)
		require.JSONEq(t, `{"error": "internal server error"}`, response.Body.String())
		require.NotContains(t, response.Body.String(), "db down")
		require.Contains(t, getLogs(), "ERROR: failed to get stats: failed to select journal sample: db down")
	})
}
