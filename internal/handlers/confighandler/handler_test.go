package confighandler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
)

func TestConfigHandler(t *testing.T) {
	var (
		source   *ConfigSourceMock
		current  *snapshot.Snapshot
		response *httptest.ResponseRecorder
		getLogs  func() string
	)

	setup := func() {
		getLogs = logger.InitTestLogger()
		current = &snapshot.Snapshot{
			ConfigID:    "c9f2a1d0e3b4",
			CollectedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			Config:      json.RawMessage(`{"placement":"B","describer_mode":"llm"}`),
		}
		source = &ConfigSourceMock{CurrentFunc: func() *snapshot.Snapshot { return current }}
		response = httptest.NewRecorder()
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		NewHandler(source).GetConfig(response, httptest.NewRequest(http.MethodGet, "/v1/config", nil))

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		require.JSONEq(t, `{"config_id":"c9f2a1d0e3b4","collected_at":"2026-11-01T10:00:00Z","config":{"placement":"B","describer_mode":"llm"}}`, response.Body.String())
	})

	t.Run("must_fail_not_collected", func(t *testing.T) {
		setup()
		current = nil

		NewHandler(source).GetConfig(response, httptest.NewRequest(http.MethodGet, "/v1/config", nil))

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.JSONEq(t, `{"error":"`+msgNotCollected+`"}`, response.Body.String())
	})

	t.Run("must_fail_internal_error_impersonal", func(t *testing.T) {
		setup()
		current.Config = json.RawMessage(`{not json`)

		NewHandler(source).GetConfig(response, httptest.NewRequest(http.MethodGet, "/v1/config", nil))

		require.Equal(t, http.StatusInternalServerError, response.Code)
		require.JSONEq(t, `{"error":"internal server error"}`, response.Body.String())
		require.Contains(t, getLogs(), "ERROR: failed to encode configuration snapshot: ")
	})
}
