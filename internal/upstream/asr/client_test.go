package asr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

const (
	contractsDir  = "../../../testdata/contracts/asr-service"
	testToken     = "asr-secret"
	testRequestID = "0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b"
)

// recordedRequest is a request the test server got.
type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// readContract reads an example of the ASR contract.
func readContract(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(contractsDir, name))
	require.NoError(t, err)
	return body
}

// respondJSON writes a JSON body with the status.
func respondJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func TestASRClient(t *testing.T) {
	var (
		m        *metrics.Metrics
		server   *httptest.Server
		client   *Client
		vars     *env.Vars
		pcm      []byte
		mu       sync.Mutex
		requests []recordedRequest
	)

	// contract serves the examples of the contract behind the token check of SVC-3.
	contract := func(t *testing.T, recognizeFile string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				respondJSON(w, http.StatusUnauthorized, []byte(`{"detail": "Invalid token"}`))
				return
			}
			switch r.URL.Path {
			case "/v1/recognize":
				respondJSON(w, http.StatusOK, readContract(t, recognizeFile))
			case "/v1/info":
				respondJSON(w, http.StatusOK, readContract(t, "info.response.json"))
			default:
				respondJSON(w, http.StatusNotFound, []byte(`{"detail": "Not Found"}`))
			}
		}
	}

	setup := func(t *testing.T, handler http.HandlerFunc) {
		logger.InitTestLogger()
		m = metrics.NewMetrics()
		requests = nil
		// 100 ms of PCM s16le mono 16 kHz, as one AUDIO_IN frame of the application
		pcm = make([]byte, 3200)
		for i := range pcm {
			pcm[i] = byte(i % 251)
		}

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests = append(requests, recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
			mu.Unlock()
			handler(w, r)
		}))
		t.Cleanup(server.Close)

		vars = &env.Vars{
			AppASRURL:       server.URL,
			AppASRToken:     testToken,
			AppASRTimeoutMS: 2000,
		}
		var err error
		client, err = NewClient(vars, m)
		require.NoError(t, err)
	}

	errorCount := func(reason string) float64 {
		return testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(upstream.NameASR, reason))
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(t, contract(t, "recognize.response.json"))

		response, err := client.Recognize(context.Background(), testRequestID, pcm)

		require.NoError(t, err)
		require.Equal(t, &RecognizeResponse{
			Text:            "Где дверь?",
			Normalized:      "где дверь",
			Language:        "ru",
			Speech:          true,
			AudioDurationMS: 1830,
			Model:           Model{Backend: "gigaam", Name: "v2_ctc", Device: "cuda"},
			Timings:         map[string]float64{"decode_ms": 1.2, "vad_ms": 4.8, "inference_ms": 38.5, "total_ms": 46.0},
			RTF:             0.025,
		}, response)
		require.Len(t, requests, 1)
		require.Equal(t, http.MethodPost, requests[0].method)
		require.Equal(t, "/v1/recognize", requests[0].path)
		require.Empty(t, requests[0].query)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
		require.Equal(t, testRequestID, requests[0].header.Get("X-Request-ID"))
		require.Equal(t, "audio/L16; rate=16000; channels=1", requests[0].header.Get("Content-Type"))
		require.Equal(t, pcm, requests[0].body)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_silence", func(t *testing.T) {
		setup(t, contract(t, "recognize.silence.response.json"))

		response, err := client.Recognize(context.Background(), testRequestID, make([]byte, 48000))

		require.NoError(t, err)
		require.False(t, response.Speech)
		require.Empty(t, response.Text)
		require.Empty(t, response.Normalized)
		require.Equal(t, 1500, response.AudioDurationMS)
		require.Equal(t, map[string]float64{"decode_ms": 0.9, "vad_ms": 4.4, "total_ms": 5.6}, response.Timings)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_info", func(t *testing.T) {
		setup(t, contract(t, "recognize.response.json"))

		info, err := client.Info(context.Background())

		require.NoError(t, err)
		require.Equal(t, &upstream.Info{
			Service:  "asr-service",
			Version:  "0.1.0",
			GitSHA:   "3f2c1ab",
			Backend:  "gigaam",
			Model:    "v2_ctc",
			Device:   "cuda",
			WarmedUp: true,
			Params:   map[string]any{"language": "ru", "sample_rate": 16000.0, "vad_enabled": true},
		}, info)
		require.Equal(t, "/v1/info", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
	})

	t.Run("must_fail_timeout", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		vars.AppASRTimeoutMS = 50
		client, _ = NewClient(vars, m)

		response, err := client.Recognize(context.Background(), testRequestID, pcm)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrTimeout)
		require.EqualError(t, err, "failed to recognize speech: upstream timeout: upstream=asr timeout=50ms")
		require.Equal(t, 1.0, errorCount(upstream.ReasonTimeout))
	})

	t.Run("must_fail_busy", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			respondJSON(w, http.StatusServiceUnavailable, []byte(`{"detail": "Service is busy"}`))
		})

		response, err := client.Recognize(context.Background(), testRequestID, pcm)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrBusy)
		require.EqualError(t, err, `failed to recognize speech: upstream busy: upstream=asr status=503 detail="Service is busy"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonBusy))
	})

	t.Run("must_fail_unavailable", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusInternalServerError, []byte(`{"detail": "Internal server error"}`))
		})

		response, err := client.Recognize(context.Background(), testRequestID, pcm)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrUnavailable)
		require.EqualError(t, err, `failed to recognize speech: upstream unavailable: upstream=asr status=500 detail="Internal server error"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnavailable))
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup(t, contract(t, "recognize.response.json"))
		vars.AppASRToken = "wrong-token"
		client, _ = NewClient(vars, m)

		response, err := client.Recognize(context.Background(), testRequestID, pcm)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrUnauthorized)
		require.EqualError(t, err, `failed to recognize speech: upstream unauthorized: upstream=asr status=401 detail="Invalid token"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnauthorized))
	})

	t.Run("must_fail_rejected", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusBadRequest, readContract(t, "error.response.json"))
		})

		response, err := client.Recognize(context.Background(), testRequestID, []byte{0x01})

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrRejected)
		require.EqualError(t, err, `failed to recognize speech: upstream rejected request: upstream=asr status=400 detail="Audio could not be decoded"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonRejected))
	})

	t.Run("must_fail_invalid_url", func(t *testing.T) {
		setup(t, contract(t, "recognize.response.json"))
		vars.AppASRURL = ""

		client, err := NewClient(vars, m)

		require.Nil(t, client)
		require.EqualError(t, err, `failed to create ASR client: invalid base URL of asr: an absolute http or https URL is required: url=""`)
	})
}
