package tts

import (
	"context"
	"errors"
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
	contractsDir  = "../../../testdata/contracts/tts-service"
	testToken     = "tts-secret"
	testRequestID = "123e4567-e89b-12d3-a456-426614174000"
	testText      = "Осторожно, впереди близко ступени."
)

// recordedRequest is a request the test server got.
type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// readContract reads an example of the TTS contract.
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

// writeAudioHeaders writes the headers of a streamed answer of TTS-1.
func writeAudioHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "audio/L16; rate=24000; channels=1")
	w.Header().Set("X-Sample-Rate", "24000")
	w.Header().Set("X-Audio-Format", "pcm_s16le")
	w.Header().Set("X-Cache", "miss")
	w.WriteHeader(http.StatusOK)
}

// waitOrCancel blocks until the channel is closed or the client gives up the request.
func waitOrCancel(r *http.Request, release <-chan struct{}) {
	select {
	case <-release:
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// readAll reads the stream to the end and returns the chunks.
func readAll(stream *Stream) ([][]byte, error) {
	chunks := [][]byte{}
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

func TestTTSClient(t *testing.T) {
	var (
		m        *metrics.Metrics
		server   *httptest.Server
		client   *Client
		vars     *env.Vars
		audio    []byte
		release  chan struct{}
		mu       sync.Mutex
		requests []recordedRequest
	)

	// contract streams the audio in two parts: 3 bytes at once, the rest after release.
	contract := func(t *testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				respondJSON(w, http.StatusUnauthorized, []byte(`{"detail": "Invalid token"}`))
				return
			}
			switch r.URL.Path {
			case "/v1/synthesize":
				writeAudioHeaders(w)
				_, _ = w.Write(audio[:3])
				w.(http.Flusher).Flush()
				waitOrCancel(r, release)
				_, _ = w.Write(audio[3:8])
				w.(http.Flusher).Flush()
				_, _ = w.Write(audio[8:])
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
		release = make(chan struct{})
		audio = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests = append(requests, recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
			mu.Unlock()
			handler(w, r)
		}))
		t.Cleanup(server.Close)

		vars = &env.Vars{
			AppTTSURL:                 server.URL,
			AppTTSToken:               testToken,
			AppTTSVoice:               "xenia",
			AppTTSFirstChunkTimeoutMS: 1000,
			AppAudioOutSampleRate:     24000,
		}
		var err error
		client, err = NewClient(vars, m)
		require.NoError(t, err)
	}

	errorCount := func(reason string) float64 {
		return testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(upstream.NameTTS, reason))
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(t, contract(t))

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)
		require.NoError(t, err)
		defer stream.Close()
		first, err := stream.Next()
		require.NoError(t, err)
		close(release)
		rest, err := readAll(stream)

		require.NoError(t, err)
		require.Equal(t, audio[:2], first)
		received := append([]byte(nil), first...)
		for _, chunk := range rest {
			require.Zero(t, len(chunk)%2)
			received = append(received, chunk...)
		}
		require.Equal(t, audio, received)
		require.Equal(t, 24000, stream.SampleRate)
		require.Equal(t, "miss", stream.Cache)
		require.Len(t, requests, 1)
		require.Equal(t, http.MethodPost, requests[0].method)
		require.Equal(t, "/v1/synthesize", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
		require.Equal(t, testRequestID, requests[0].header.Get("X-Request-ID"))
		require.Equal(t, "application/json", requests[0].header.Get("Content-Type"))
		require.JSONEq(t, string(readContract(t, "synthesize.request.json")), string(requests[0].body))
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_slow_rest_of_stream", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			writeAudioHeaders(w)
			_, _ = w.Write(audio[:4])
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write(audio[4:])
		})
		vars.AppTTSFirstChunkTimeoutMS = 50
		client, _ = NewClient(vars, m)

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)
		require.NoError(t, err)
		defer stream.Close()
		chunks, err := readAll(stream)

		require.NoError(t, err)
		received := []byte{}
		for _, chunk := range chunks {
			received = append(received, chunk...)
		}
		require.Equal(t, audio, received)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_default_voice", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			writeAudioHeaders(w)
			_, _ = w.Write(audio)
		})
		vars.AppTTSVoice = ""
		client, _ = NewClient(vars, m)

		stream, err := client.Synthesize(context.Background(), testRequestID, "Впереди стол.")
		require.NoError(t, err)
		defer stream.Close()
		_, err = readAll(stream)

		require.NoError(t, err)
		require.JSONEq(t, `{"text":"Впереди стол.","sample_rate":24000,"format":"pcm_s16le","speed":1.0}`, string(requests[0].body))
	})

	t.Run("must_pass_info", func(t *testing.T) {
		setup(t, contract(t))

		info, err := client.Info(context.Background())

		require.NoError(t, err)
		require.Equal(t, &upstream.Info{
			Service:  "tts-service",
			Version:  "0.1.0",
			GitSHA:   "3f2c1ab",
			Backend:  "silero",
			Model:    "v4_ru",
			Device:   "cpu",
			WarmedUp: true,
			Params:   map[string]any{"voice": "xenia", "sample_rate": 48000.0, "cache_max_items": 256.0},
		}, info)
		require.Equal(t, "/v1/info", requests[0].path)
		require.Equal(t, "Bearer "+testToken, requests[0].header.Get("Authorization"))
	})

	t.Run("must_fail_timeout", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			waitOrCancel(r, release)
		})
		vars.AppTTSFirstChunkTimeoutMS = 50
		client, _ = NewClient(vars, m)

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrTimeout)
		require.EqualError(t, err, "failed to synthesize speech: upstream timeout: upstream=tts timeout=50ms")
		require.Equal(t, 1.0, errorCount(upstream.ReasonTimeout))
	})

	t.Run("must_fail_timeout_first_chunk_after_headers", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			writeAudioHeaders(w)
			w.(http.Flusher).Flush()
			waitOrCancel(r, release)
		})
		vars.AppTTSFirstChunkTimeoutMS = 50
		client, _ = NewClient(vars, m)

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrTimeout)
		require.EqualError(t, err, "failed to synthesize speech: upstream timeout: upstream=tts timeout=50ms")
		require.Equal(t, 1.0, errorCount(upstream.ReasonTimeout))
	})

	t.Run("must_fail_busy", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			respondJSON(w, http.StatusServiceUnavailable, []byte(`{"detail": "Service is busy"}`))
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrBusy)
		require.EqualError(t, err, `failed to synthesize speech: upstream busy: upstream=tts status=503 detail="Service is busy"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonBusy))
	})

	t.Run("must_fail_unavailable", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusInternalServerError, []byte(`{"detail": "Internal server error"}`))
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrUnavailable)
		require.EqualError(t, err, `failed to synthesize speech: upstream unavailable: upstream=tts status=500 detail="Internal server error"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnavailable))
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup(t, contract(t))
		vars.AppTTSToken = "wrong-token"
		client, _ = NewClient(vars, m)

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrUnauthorized)
		require.EqualError(t, err, `failed to synthesize speech: upstream unauthorized: upstream=tts status=401 detail="Invalid token"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnauthorized))
	})

	t.Run("must_fail_rejected", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusUnprocessableEntity, readContract(t, "error.response.json"))
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrRejected)
		require.EqualError(t, err, `failed to synthesize speech: upstream rejected request: upstream=tts status=422 detail="Unknown voice"`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonRejected))
	})

	t.Run("must_fail_stream_interrupted", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			writeAudioHeaders(w)
			_, _ = w.Write(audio)
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)
		require.NoError(t, err)
		defer stream.Close()
		chunks, err := readAll(stream)

		require.Equal(t, [][]byte{audio}, chunks)
		require.ErrorIs(t, err, upstream.ErrUnavailable)
		require.EqualError(t, err, "upstream unavailable: upstream=tts: unexpected EOF")
		require.Equal(t, 1.0, errorCount(upstream.ReasonUnavailable))
	})

	t.Run("must_fail_empty_stream", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			writeAudioHeaders(w)
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrInvalidResponse)
		require.EqualError(t, err, "failed to synthesize speech: invalid upstream response: upstream=tts: empty stream")
		require.Equal(t, 1.0, errorCount(upstream.ReasonInvalidResponse))
	})

	t.Run("must_fail_invalid_content_type", func(t *testing.T) {
		setup(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, []byte(`{"status": "ok"}`))
		})

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)

		require.Nil(t, stream)
		require.ErrorIs(t, err, upstream.ErrInvalidResponse)
		require.EqualError(t, err, `failed to synthesize speech: invalid upstream response: upstream=tts content_type="application/json" expected=audio/L16`)
		require.Equal(t, 1.0, errorCount(upstream.ReasonInvalidResponse))
	})

	t.Run("must_fail_cancelled_during_stream", func(t *testing.T) {
		setup(t, contract(t))
		ctx, cancel := context.WithCancel(context.Background())

		stream, err := client.Synthesize(ctx, testRequestID, testText)
		require.NoError(t, err)
		defer stream.Close()
		_, err = stream.Next()
		require.NoError(t, err)
		cancel()
		_, err = stream.Next()

		require.ErrorIs(t, err, context.Canceled)
		require.EqualError(t, err, "upstream call interrupted: upstream=tts: context canceled")
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_fail_closed_stream", func(t *testing.T) {
		setup(t, contract(t))

		stream, err := client.Synthesize(context.Background(), testRequestID, testText)
		require.NoError(t, err)
		_, err = stream.Next()
		require.NoError(t, err)
		stream.Close()
		stream.Close()
		_, err = stream.Next()

		require.EqualError(t, err, "upstream call interrupted: upstream=tts: stream closed")
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})
}
