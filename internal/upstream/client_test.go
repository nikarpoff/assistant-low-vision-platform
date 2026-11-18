package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
)

func TestNewClient(t *testing.T) {
	var (
		m      *metrics.Metrics
		config Config
	)

	setup := func() {
		logger.InitTestLogger()
		m = metrics.NewMetrics()
		config = Config{
			Name:    NameDetector,
			BaseURL: "http://detector:8000/",
			Token:   "secret",
			Timeout: 400 * time.Millisecond,
		}
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		client, err := NewClient(config, m)

		require.NoError(t, err)
		require.Equal(t, NameDetector, client.Name())
		require.Equal(t, "http://detector:8000", client.baseURL)
		require.Equal(t, 400*time.Millisecond, client.timeout)
		transport, ok := client.httpClient.Transport.(*http.Transport)
		require.True(t, ok)
		require.Equal(t, maxIdleConnsPerHost, transport.MaxIdleConnsPerHost)
		require.True(t, transport.DisableCompression)
		require.Nil(t, transport.Proxy)
	})

	t.Run("must_fail_invalid_base_url", func(t *testing.T) {
		setup()
		config.BaseURL = "detector:8000"

		client, err := NewClient(config, m)

		require.Nil(t, client)
		require.EqualError(t, err, `invalid base URL of detector: an absolute http or https URL is required: url="detector:8000"`)
	})

	t.Run("must_fail_unsupported_scheme", func(t *testing.T) {
		setup()
		config.BaseURL = "ftp://detector:8000"

		client, err := NewClient(config, m)

		require.Nil(t, client)
		require.EqualError(t, err, `invalid base URL of detector: an absolute http or https URL is required: url="ftp://detector:8000"`)
	})

	t.Run("must_fail_invalid_timeout", func(t *testing.T) {
		setup()
		config.Timeout = 0

		client, err := NewClient(config, m)

		require.Nil(t, client)
		require.EqualError(t, err, "invalid timeout of detector: a positive duration is required: timeout=0s")
	})
}

func TestClient_Call(t *testing.T) {
	var (
		m        *metrics.Metrics
		server   *httptest.Server
		client   *Client
		response map[string]any
	)

	setup := func(t *testing.T, handler http.HandlerFunc) {
		logger.InitTestLogger()
		m = metrics.NewMetrics()
		server = httptest.NewServer(handler)
		t.Cleanup(server.Close)
		var err error
		client, err = NewClient(Config{Name: NameASR, BaseURL: server.URL, Timeout: time.Second}, m)
		require.NoError(t, err)
		response = map[string]any{}
	}

	t.Run("must_pass_request_timeout_overrides_client", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})

		err := client.Call(context.Background(), &Request{Method: http.MethodGet, Path: pathInfo, Timeout: 30 * time.Millisecond}, &response)

		require.ErrorIs(t, err, ErrTimeout)
		require.EqualError(t, err, "upstream timeout: upstream=asr timeout=30ms")
	})

	t.Run("must_fail_response_too_large", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ContentTypeJSON)
			_, _ = w.Write(make([]byte, maxResponseBytes+1))
		})

		err := client.Call(context.Background(), &Request{Method: http.MethodGet, Path: pathInfo}, &response)

		require.ErrorIs(t, err, ErrInvalidResponse)
		require.EqualError(t, err, "invalid upstream response: upstream=asr: body exceeds 1048576 bytes")
		require.Equal(t, 1.0, testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(NameASR, ReasonInvalidResponse)))
	})

	t.Run("must_fail_not_json_error_body", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		})

		err := client.Call(context.Background(), &Request{Method: http.MethodGet, Path: pathInfo}, &response)

		require.ErrorIs(t, err, ErrUnavailable)
		require.EqualError(t, err, `upstream unavailable: upstream=asr status=502 detail=""`)
		require.Equal(t, 1.0, testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(NameASR, ReasonUnavailable)))
	})
}

func TestHasMediaType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		expected    string
		ok          bool
	}{
		{"must_pass_exact", "application/json", "application/json", true},
		{"must_pass_with_parameters", "audio/L16; rate=24000; channels=1", "audio/L16", true},
		{"must_pass_case_insensitive", "audio/l16;rate=24000", "audio/L16", true},
		{"must_fail_other_type", "text/html; charset=utf-8", "application/json", false},
		{"must_fail_empty", "", "application/json", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.ok, hasMediaType(tt.contentType, tt.expected))
		})
	}
}
