package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/journal"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// Markers inside the snapshot and the question audio: a record or a log line holding them holds the media.
const (
	imageMarker = "JPEG-MEDIA-MARKER-5c1e9a"
	audioMarker = "PCM-MEDIA-MARKER-7d2f0b"
)

// markedImage is a fake JPEG snapshot with imageMarker inside.
func markedImage() []byte {
	image := append([]byte{}, testImage[:len(testImage)-2]...)
	image = append(image, []byte(imageMarker)...)
	return append(image, 0xFF, 0xD9)
}

// markedAudio is 300 ms of PCM s16le 16 kHz with audioMarker inside.
func markedAudio() []byte {
	audio := make([]byte, 9600)
	copy(audio[4000:], audioMarker)
	return audio
}

// ask sends a question with a snapshot and its audio and reads everything up to its answer.end or error.
func ask(t *testing.T, conn *websocket.Conn, id uuid.UUID, image, audio []byte) []received {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"type":"ask.start","request_id":"%s","trigger":"hold","with_image":true}`, id))))
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: image})))
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindAudioIn, RequestID: id, Payload: audio})))
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"type":"ask.end","request_id":"%s","audio_ms":%d}`, id, len(audio)/32))))

	all := []received{}
	for {
		got := read(t, conn)
		all = append(all, got)
		switch got.message.(type) {
		case *protocol.AnswerEnd, *protocol.ErrorMessage:
			return all
		}
	}
}

// describeImage sends a describe request with the snapshot and reads up to its answer.end or error.
func describeImage(t *testing.T, conn *websocket.Conn, id uuid.UUID, image []byte) []received {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"type":"describe","request_id":"%s","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":%d}}`, id, len(image)))))
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: image})))

	all := []received{}
	for {
		got := read(t, conn)
		all = append(all, got)
		switch got.message.(type) {
		case *protocol.AnswerEnd, *protocol.ErrorMessage:
			return all
		}
	}
}

// waitRequests waits until the journal has n request records.
func waitRequests(t *testing.T, g *gateway, n int) []*journal.Request {
	t.Helper()
	require.Eventually(t, func() bool { return len(g.records.requests()) >= n }, testTimeout, 10*time.Millisecond)
	return g.records.requests()
}

// dump renders the journal records as JSON and as Go values.
func dump(t *testing.T, all []journal.Record) string {
	t.Helper()
	var out strings.Builder
	for _, record := range all {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		out.Write(encoded)
		fmt.Fprintf(&out, "\n%+v\n", record)
	}
	return out.String()
}

// get sends GET to the gateway with the admin token if it is not empty.
func get(t *testing.T, g *gateway, path, token string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, g.http.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, body
}

func TestJournal(t *testing.T) {
	var (
		g    *gateway
		conn *websocket.Conn
	)

	setup := func(environment map[string]string, llmCfg fakellm.Config) {
		logger.InitTestLogger()
		g = newGatewayWith(t, environment, llmCfg)
		conn = g.open(t)
	}

	t.Run("must_pass_media_never_stored", func(t *testing.T) {
		tmp, home, work := t.TempDir(), t.TempDir(), t.TempDir()
		t.Setenv("TMPDIR", tmp)
		t.Setenv("HOME", home)
		t.Chdir(work)
		setup(nil, fakellm.Config{})
		image, audio := markedImage(), markedAudio()

		described := describeImage(t, conn, uuid.New(), image)
		asked := ask(t, conn, uuid.New(), image, audio)

		require.IsType(t, &protocol.AnswerEnd{}, described[len(described)-1].message)
		require.IsType(t, &protocol.AnswerEnd{}, asked[len(asked)-1].message)
		requests := waitRequests(t, g, 2)
		require.Equal(t, len(image), requests[0].ImageBytes)
		require.Equal(t, len(image), requests[1].ImageBytes)
		for _, dir := range []string{tmp, home, work} {
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, filepath.Base(dir))
		}
		all := dump(t, g.records.all())
		for _, media := range [][]byte{image, audio} {
			require.False(t, bytes.Contains([]byte(all), media))
			require.NotContains(t, all, base64.StdEncoding.EncodeToString(media))
			require.NotContains(t, all, base64.RawURLEncoding.EncodeToString(media))
		}
		require.NotContains(t, all, imageMarker)
		require.NotContains(t, all, audioMarker)
	})

	t.Run("must_pass_llm_fields", func(t *testing.T) {
		setup(nil, fakellm.Config{})
		id := uuid.New()

		describeImage(t, conn, id, testImage)

		request := waitRequests(t, g, 1)[0]
		require.Equal(t, id.String(), request.RequestID)
		require.Equal(t, pipeline.KindDescribe, request.Kind)
		require.Equal(t, "llm", request.DescriberMode)
		require.Equal(t, pipeline.StatusOK, request.Status)
		require.Equal(t, fakellm.DefaultModel, request.LLMModel)
		require.Equal(t, fakellm.SystemFingerprint, request.LLMSystemFingerprint)
		require.Empty(t, request.LLMError)
		require.Empty(t, request.DegradedReason)
		require.Equal(t, len(testImage), request.ImageBytes)
		require.Equal(t, g.app.collector.Current().ConfigID, request.ConfigID)
		require.Contains(t, request.Timings, pipeline.StageLLMTTFT)

		// The session and the configuration snapshot are in the journal too
		var snapshot *journal.ConfigSnapshot
		var opened *journal.Session
		for _, record := range g.records.all() {
			switch typed := record.(type) {
			case *journal.ConfigSnapshot:
				snapshot = typed
			case *journal.Session:
				opened = typed
			}
		}
		require.NotNil(t, snapshot)
		require.Equal(t, request.ConfigID, snapshot.ConfigID)
		require.JSONEq(t, string(g.app.collector.Current().Config), string(snapshot.Config))
		require.NotNil(t, opened)
		require.Equal(t, request.SessionID, opened.SessionID)

		// A failure of the provider: the kind of the failure and the degraded reason
		for _, tt := range []struct {
			fail   fakellm.FailMode
			reason llm.Reason
		}{
			{fakellm.FailHTTP402, llm.ReasonInsufficientBalance},
			{fakellm.FailStream402AfterSentence, llm.ReasonInsufficientBalance},
			{fakellm.FailHTTP500, llm.ReasonHTTP5xx},
		} {
			setup(nil, fakellm.Config{Fail: tt.fail})
			id := uuid.New()

			end := describeImage(t, conn, id, testImage)

			require.True(t, end[len(end)-1].message.(*protocol.AnswerEnd).Degraded, tt.fail)
			request := waitRequests(t, g, 1)[0]
			require.Equal(t, pipeline.StatusDegraded, request.Status, tt.fail)
			require.Equal(t, string(protocol.DegradedLLMUnavailable), request.DegradedReason, tt.fail)
			require.Equal(t, string(tt.reason), request.LLMError, tt.fail)
			require.Equal(t, len(testImage), request.ImageBytes, tt.fail)
		}
	})
}

func TestMetrics(t *testing.T) {
	var (
		g    *gateway
		conn *websocket.Conn
	)

	setup := func(llmCfg fakellm.Config) {
		logger.InitTestLogger()
		g = newGatewayWith(t, nil, llmCfg)
		conn = g.open(t)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(fakellm.Config{Fail: fakellm.FailHTTP500})
		id := uuid.New()
		describeImage(t, conn, id, testImage)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
			`{"type":"client.metrics","request_id":"%s","network":"wifi","metrics":{"trigger_to_first_audio_ms":1310}}`, id))))
		require.Eventually(t, func() bool {
			_, body := get(t, g, "/metrics", "")
			return strings.Contains(string(body), `alv_e2e_duration_seconds_count{kind="describe",network="wifi"} 1`)
		}, testTimeout, 10*time.Millisecond)

		code, body := get(t, g, "/metrics", "")

		require.Equal(t, http.StatusOK, code)
		text := string(body)
		require.Contains(t, text, `alv_stage_duration_seconds_count{kind="describe",stage="detect_ms"} 1`)
		require.Contains(t, text, `alv_stage_duration_seconds_count{kind="describe",stage="server_total_ms"} 1`)
		require.Contains(t, text, `alv_requests_total{kind="describe",status="degraded"} 1`)
		require.Contains(t, text, `alv_upstream_errors_total{reason="http_5xx",upstream="llm"} 1`)
		require.Contains(t, text, `alv_sessions_active 1`)
		require.Contains(t, text, `alv_journal_dropped_total 0`)
	})
}

func TestConfigEndpoint(t *testing.T) {
	var (
		g *gateway
	)

	setup := func() {
		logger.InitTestLogger()
		g = newGateway(t, map[string]string{"APP_PLACEMENT": "A"})
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		code, body := get(t, g, "/v1/config", testAdminToken)

		require.Equal(t, http.StatusOK, code)
		var response struct {
			ConfigID    string          `json:"config_id"`
			CollectedAt string          `json:"collected_at"`
			Config      json.RawMessage `json:"config"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.Regexp(t, `^[0-9a-f]{12}$`, response.ConfigID)
		require.Equal(t, g.app.collector.Current().ConfigID, response.ConfigID)
		collectedAt, err := time.Parse(time.RFC3339Nano, response.CollectedAt)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(response.CollectedAt, "Z"), response.CollectedAt)
		require.WithinDuration(t, time.Now(), collectedAt, time.Minute)
		var config map[string]any
		require.NoError(t, json.Unmarshal(response.Config, &config))
		require.Equal(t, "A", config["placement"])
		require.Equal(t, "llm", config["describer_mode"])
		require.Contains(t, config, "llm")
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup()

		for _, token := range []string{"", "wrong-token", testToken} {
			code, body := get(t, g, "/v1/config", token)

			require.Equal(t, http.StatusUnauthorized, code, token)
			require.JSONEq(t, `{"error":"unauthorized"}`, string(body))
		}
		code, _ := get(t, g, "/v1/stats", "")
		require.Equal(t, http.StatusUnauthorized, code)
	})
}

func TestSecretsNotExposed(t *testing.T) {
	const (
		llmKey        = "sk-llm-secret-4f8a1c"
		deviceToken   = "device-secret-9b3e7d"
		adminToken    = "admin-secret-2c6f0a"
		detectorToken = "detector-secret-8e1b5f"
		asrToken      = "asr-secret-3a9d4c"
		ttsToken      = "tts-secret-6d2e8b"
		dbPassword    = "pg-secret-7c4a1e"
	)
	secrets := []string{llmKey, deviceToken, adminToken, detectorToken, asrToken, ttsToken, dbPassword}

	var (
		g       *gateway
		conn    *websocket.Conn
		getLogs func() string
	)

	setup := func(fail fakellm.FailMode) {
		getLogs = logger.InitTestLogger()
		g = newGatewayWith(t, map[string]string{
			"APP_LOG_LEVEL":      "DEBUG",
			"APP_LLM_API_KEY":    llmKey,
			"APP_DEVICE_TOKENS":  deviceToken,
			"APP_ADMIN_TOKEN":    adminToken,
			"APP_DETECTOR_TOKEN": detectorToken,
			"APP_ASR_TOKEN":      asrToken,
			"APP_TTS_TOKEN":      ttsToken,
			// The database is not there: the journal fails, and its errors are logged
			"APP_POSTGRES_DSN": "postgres://alv:" + dbPassword + "@127.0.0.1:1/alv?sslmode=disable&connect_timeout=1",
		}, fakellm.Config{APIKey: llmKey, Fail: fail})
		conn = g.open(t)
	}

	requireNoSecrets := func(t *testing.T, where, text string) {
		for _, secret := range secrets {
			require.NotContains(t, text, secret, where)
		}
	}

	t.Run("must_pass", func(t *testing.T) {
		for _, fail := range []fakellm.FailMode{fakellm.FailNone, fakellm.FailHTTP402, fakellm.FailStreamErrorAfterSentence} {
			setup(fail)

			describeImage(t, conn, uuid.New(), testImage)
			ask(t, conn, uuid.New(), testImage, markedAudio())
			_, configBody := get(t, g, "/v1/config", adminToken)
			_, statsBody := get(t, g, "/v1/stats", adminToken)
			_, readyBody := get(t, g, "/ready", "")
			_, unauthorizedBody := get(t, g, "/v1/config", "")
			waitRequests(t, g, 2)
			g.app.Stop()

			// The provider got the key: the check is not vacuous
			require.Equal(t, "Bearer "+llmKey, completions(g.llm)[0].Header.Get("Authorization"))
			snapshot, err := json.Marshal(g.app.collector.Current())
			require.NoError(t, err)
			requireNoSecrets(t, "snapshot", string(snapshot))
			requireNoSecrets(t, "/v1/config", string(configBody))
			requireNoSecrets(t, "/v1/stats", string(statsBody))
			requireNoSecrets(t, "/ready", string(readyBody))
			requireNoSecrets(t, "401", string(unauthorizedBody))
			requireNoSecrets(t, "journal", dump(t, g.records.all()))
			logs := getLogs()
			require.Contains(t, logs, "DEBUG: ")
			require.Contains(t, logs, "failed to migrate journal database")
			requireNoSecrets(t, "log", logs)
		}
	})
}

func TestAppReady(t *testing.T) {
	var (
		g *gateway
	)

	setup := func(environment map[string]string) {
		logger.InitTestLogger()
		g = newGateway(t, environment)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(nil)

		require.Eventually(t, func() bool { return g.app.provider.State() == journal.StateOK }, testTimeout, 10*time.Millisecond)
		code, body := get(t, g, "/ready", "")

		require.Equal(t, http.StatusOK, code)
		require.JSONEq(t, `{"status":"ready","detector":"ok","asr":"ok","tts":"ok","llm":"ok","journal":"disabled","vocabulary":"loaded"}`, string(body))
		models := 0
		for _, request := range g.llm.Requests() {
			if request.Path == fakellm.PathModels {
				models++
				require.Equal(t, http.MethodGet, request.Method)
			}
		}
		require.Equal(t, 1, models)
	})

	t.Run("must_pass_llm_down", func(t *testing.T) {
		setup(map[string]string{"APP_LLM_URL": "http://127.0.0.1:1/v1"})

		require.Eventually(t, func() bool {
			code, body := get(t, g, "/ready", "")
			return code == http.StatusOK && strings.Contains(string(body), `"llm":"down"`)
		}, testTimeout, 10*time.Millisecond)
	})

	t.Run("must_pass_template_llm_disabled", func(t *testing.T) {
		setup(map[string]string{"APP_DESCRIBER_MODE": "template"})

		code, body := get(t, g, "/ready", "")

		require.Equal(t, http.StatusOK, code)
		require.Contains(t, string(body), `"llm":"disabled"`)
	})

	t.Run("must_fail_detector_down", func(t *testing.T) {
		setup(nil)
		g.detector.Close()

		code, body := get(t, g, "/ready", "")

		require.Equal(t, http.StatusServiceUnavailable, code)
		require.Contains(t, string(body), `"detector":"down"`)
	})

	t.Run("must_pass_vlm_without_detector", func(t *testing.T) {
		setup(map[string]string{"APP_DESCRIBER_MODE": "vlm"})
		g.detector.Close()

		code, _ := get(t, g, "/ready", "")

		require.Equal(t, http.StatusOK, code)
	})
}
