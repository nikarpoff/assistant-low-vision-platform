package detector

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

const (
	contractsDir  = "../../../testdata/contracts/objects-detector"
	testToken     = "detector-secret"
	testRequestID = "123e4567-e89b-12d3-a456-426614174000"
)

// recordedRequest is a request the test server got.
type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// readContract reads an example of the detector contract.
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

// waitForCancel blocks until the client gives up the request.
func waitForCancel(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func TestDetectorClient(t *testing.T) {
	var (
		m           *metrics.Metrics
		server      *httptest.Server
		client      *Client
		vars        *env.Vars
		image       []byte
		mu          sync.Mutex
		requests    []recordedRequest
		connections atomic.Int32
	)

	// contract serves the examples of the contract behind the token check of SVC-3.
	contract := func(t *testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				respondJSON(w, http.StatusUnauthorized, []byte(`{"detail": "Invalid token"}`))
				return
			}
			switch r.URL.Path {
			case "/v1/detect":
				respondJSON(w, http.StatusOK, readContract(t, "detect.response.json"))
			case "/v1/vocabulary":
				respondJSON(w, http.StatusOK, readContract(t, "vocabulary.response.json"))
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
		connections.Store(0)
		image = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}

		server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests = append(requests, recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
			mu.Unlock()
			handler(w, r)
		}))
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		server.Start()
		t.Cleanup(server.Close)

		vars = &env.Vars{
			AppDetectorURL:     server.URL,
			AppDetectorToken:   testToken,
			AppDetectTimeoutMS: 400,
		}
		var err error
		client, err = NewClient(vars, m)
		require.NoError(t, err)
	}

	errorCount := func(reason string) float64 {
		return testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(upstream.NameDetector, reason))
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(t, contract(t))

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.NoError(t, err)
		require.Equal(t, &DetectResponse{
			Image: ImageSize{Width: 640, Height: 480},
			Objects: []Object{
				{
					Label:      "stairs",
					LabelRU:    "ступени",
					Confidence: 0.81,
					Box:        Box{X1: 0.3, Y1: 0.55, X2: 0.72, Y2: 1.0},
					Direction:  DirectionAhead,
					Distance:   DistanceNear,
					Area:       0.189,
					Key:        true,
				},
				{
					Label:      "chair",
					LabelRU:    "стул",
					Confidence: 0.64,
					Box:        Box{X1: 0.02, Y1: 0.4, X2: 0.2, Y2: 0.75},
					Direction:  DirectionLeft,
					Distance:   DistanceFar,
					Area:       0.063,
					Key:        true,
				},
			},
			Model:   Model{Backend: "ultralytics", Name: "yoloe-11s-seg", Imgsz: 640, Device: "cuda"},
			Timings: map[string]float64{"decode_ms": 2.1, "inference_ms": 11.6, "postprocess_ms": 0.7, "total_ms": 15.0},
		}, response)
		require.Len(t, requests, 1)
		require.Equal(t, http.MethodPost, requests[0].method)
		require.Equal(t, "/v1/detect", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
		require.Equal(t, testRequestID, requests[0].header.Get("X-Request-ID"))
		require.Equal(t, "image/jpeg", requests[0].header.Get("Content-Type"))
		require.Equal(t, image, requests[0].body)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_vocabulary", func(t *testing.T) {
		setup(t, contract(t))

		document, err := client.Vocabulary(context.Background())

		require.NoError(t, err)
		require.Equal(t, "8778f20c71d0", document.Hash)
		require.Len(t, document.Classes, 26)
		require.Equal(t, vocabulary.Class{
			ID:       "door",
			Prompts:  []string{"door"},
			RU:       vocabulary.Forms{One: "дверь", Few: "двери", Many: "дверей", Gender: vocabulary.GenderFeminine},
			Key:      true,
			Mentions: []string{"двер(?:ь|и|ью|ей|ям|ями|ях)"},
		}, document.Classes[1])
		_, err = vocabulary.NewVocabulary(document)
		require.NoError(t, err)
		require.Len(t, requests, 1)
		require.Equal(t, http.MethodGet, requests[0].method)
		require.Equal(t, "/v1/vocabulary", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
		require.Empty(t, requests[0].header.Get("X-Request-ID"))
	})

	t.Run("must_pass_info", func(t *testing.T) {
		setup(t, contract(t))

		info, err := client.Info(context.Background())

		require.NoError(t, err)
		require.Equal(t, &upstream.Info{
			Service:  "objects-detector",
			Version:  "0.1.0",
			GitSHA:   "3f2c1ab",
			Backend:  "ultralytics",
			Model:    "yoloe-11s-seg",
			Device:   "cuda",
			WarmedUp: true,
			Params:   map[string]any{"imgsz": 640.0, "conf": 0.35, "vocabulary_hash": "a1b2c3d4e5f6"},
		}, info)
		require.NotContains(t, info.Params, "unsupported_classes")
		require.Len(t, requests, 1)
		require.Equal(t, "/v1/info", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
	})

	t.Run("must_pass_keep_alive", func(t *testing.T) {
		setup(t, contract(t))

		for i := 0; i < 3; i++ {
			_, err := client.Detect(context.Background(), testRequestID, image)
			require.NoError(t, err)
		}
		_, err := client.Info(context.Background())

		require.NoError(t, err)
		require.Len(t, requests, 4)
		require.Equal(t, int32(1), connections.Load())
	})

	t.Run("must_pass_without_token", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, readContract(t, "detect.response.json"))
		})
		vars.AppDetectorToken = ""
		client, _ = NewClient(vars, m)

		_, err := client.Detect(context.Background(), testRequestID, image)

		require.NoError(t, err)
		require.Len(t, requests, 1)
		require.NotContains(t, requests[0].header, "Authorization")
	})

	t.Run("must_fail_timeout", func(t *testing.T) {
		setup(t, waitForCancel)
		vars.AppDetectTimeoutMS = 50
		client, _ = NewClient(vars, m)

		started := time.Now()
		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrTimeout)
		require.EqualError(t, err, "failed to detect objects: upstream timeout: upstream=detector timeout=50ms")
		require.Less(t, time.Since(started), 2*time.Second)
		require.Equal(t, 1.0, errorCount(upstream.ReasonTimeout))
	})

	t.Run("must_fail_busy", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			respondJSON(w, http.StatusServiceUnavailable, []byte(`{"detail": "Service is busy"}`))
		})

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrBusy)
		require.EqualError(t, err, `failed to detect objects: upstream busy: upstream=detector status=503 detail="Service is busy"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonBusy))
		require.Len(t, requests, 1)
	})

	t.Run("must_fail_unavailable", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusInternalServerError, []byte(`{"detail": "Internal server error"}`))
		})

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrUnavailable)
		require.EqualError(t, err, `failed to detect objects: upstream unavailable: upstream=detector status=500 detail="Internal server error"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnavailable))
	})

	t.Run("must_fail_connection_refused", func(t *testing.T) {
		setup(t, contract(t))
		server.Close()
		address := server.Listener.Addr().String()

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrUnavailable)
		require.EqualError(t, err, fmt.Sprintf(
			`failed to detect objects: upstream unavailable: upstream=detector: Post "%s/v1/detect": dial tcp %s: connect: connection refused`,
			server.URL, address))
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnavailable))
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup(t, contract(t))
		vars.AppDetectorToken = "wrong-token"
		client, _ = NewClient(vars, m)

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrUnauthorized)
		require.EqualError(t, err, `failed to detect objects: upstream unauthorized: upstream=detector status=401 detail="Invalid token"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnauthorized))
		require.Equal(t, "Bearer wrong-token", requests[0].header.Get("Authorization"))
	})

	t.Run("must_fail_rejected", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusBadRequest, readContract(t, "error.response.json"))
		})

		response, err := client.Detect(context.Background(), testRequestID, []byte("not an image"))

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrRejected)
		require.EqualError(t, err, `failed to detect objects: upstream rejected request: upstream=detector status=400 detail="Image could not be decoded"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonRejected))
	})

	t.Run("must_fail_invalid_content_type", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html></html>"))
		})

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrInvalidResponse)
		require.EqualError(t, err, `failed to detect objects: invalid upstream response: upstream=detector content_type="text/html; charset=utf-8" expected=application/json`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonInvalidResponse))
	})

	t.Run("must_fail_invalid_json", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, []byte("not json"))
		})

		response, err := client.Detect(context.Background(), testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, upstream.ErrInvalidResponse)
		require.EqualError(t, err, "failed to detect objects: invalid upstream response: upstream=detector: invalid character 'o' in literal null (expecting 'u')")
		require.Equal(t, 1.0, errorCount(upstream.ReasonInvalidResponse))
	})

	t.Run("must_fail_cancelled", func(t *testing.T) {
		setup(t, waitForCancel)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)

		response, err := client.Detect(ctx, testRequestID, image)

		require.Nil(t, response)
		require.ErrorIs(t, err, context.Canceled)
		require.EqualError(t, err, "failed to detect objects: upstream call interrupted: upstream=detector: context canceled")
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_fail_invalid_url", func(t *testing.T) {
		setup(t, contract(t))
		vars.AppDetectorURL = "detector:8000"

		client, err := NewClient(vars, m)

		require.Nil(t, client)
		require.EqualError(t, err, `failed to create detector client: invalid base URL of detector: an absolute http or https URL is required: url="detector:8000"`)
	})
}
