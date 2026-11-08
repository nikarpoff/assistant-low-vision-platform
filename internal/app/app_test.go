package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/servicestub"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/session"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// Settings of the tests.
const (
	testToken      = "device-token-1"
	testAdminToken = "admin-token-1"
	testModel      = "qwen/qwen3-8b"
	testTimeout    = 5 * time.Second
	testHello      = `{"type":"hello","protocol":1,"device_id":"5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10","app_version":"0.1.0","locale":"ru-RU","platform":"android-34","network":"wifi"}`
)

// testImage is a small fake JPEG snapshot.
var testImage = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}

// gateway is the whole gateway in-process over the service stubs and the provider stub.
type gateway struct {
	app      *App
	http     *httptest.Server
	detector *servicestub.Detector
	tts      *servicestub.TTS
	llm      *fakellm.Server
}

// newGateway starts the gateway from the environment, as cmd/gateway does.
func newGateway(t *testing.T, environment map[string]string) *gateway {
	t.Helper()
	g := &gateway{
		detector: servicestub.NewDetector(),
		tts:      servicestub.NewTTS(),
		llm:      fakellm.NewServer(fakellm.Config{}),
	}
	t.Cleanup(g.detector.Close)
	t.Cleanup(g.tts.Close)
	llmServer := httptest.NewServer(g.llm)
	t.Cleanup(llmServer.Close)

	defaults := map[string]string{
		"APP_DEVICE_TOKENS":  testToken,
		"APP_ADMIN_TOKEN":    testAdminToken,
		"APP_DESCRIBER_MODE": "llm",
		"APP_LLM_URL":        llmServer.URL + fakellm.BasePath,
		"APP_LLM_MODEL":      testModel,
		"APP_DETECTOR_URL":   g.detector.URL,
		"APP_ASR_URL":        "http://127.0.0.1:1",
		"APP_TTS_URL":        g.tts.URL,
	}
	for name, value := range environment {
		defaults[name] = value
	}
	for name, value := range defaults {
		t.Setenv(name, value)
	}
	vars, err := env.Load()
	require.NoError(t, err)

	g.app, err = NewApp(vars)
	require.NoError(t, err)
	require.NoError(t, g.app.Start(context.Background()))
	t.Cleanup(g.app.Stop)
	select {
	case <-g.app.Ready():
	case <-time.After(testTimeout):
		t.Fatal("gateway is not ready")
	}
	g.http = httptest.NewServer(g.app.Handler())
	t.Cleanup(g.http.Close)
	return g
}

// open opens a session and says hello.
func (g *gateway) open(t *testing.T) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{session.Subprotocol}, HandshakeTimeout: testTimeout}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(g.http.URL, "http")+session.Path, http.Header{"Authorization": {"Bearer " + testToken}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(testHello)))
	require.IsType(t, &protocol.Welcome{}, read(t, conn).message)
	return conn
}

// received is a message or a frame from the gateway.
type received struct {
	message protocol.Message
	frame   *protocol.Frame
}

func read(t *testing.T, conn *websocket.Conn) received {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	messageType, data, err := conn.ReadMessage()
	require.NoError(t, err)
	if messageType == websocket.BinaryMessage {
		frame, err := protocol.DecodeFrame(data, protocol.NewFrameLimits(0))
		require.NoError(t, err)
		return received{frame: frame}
	}
	message, err := protocol.DecodeServerMessage(data)
	require.NoError(t, err, string(data))
	return received{message: message}
}

// describe sends a describe request and reads everything up to its answer.end or error.
func describe(t *testing.T, conn *websocket.Conn, id uuid.UUID) []received {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"type":"describe","request_id":"%s","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":%d}}`, id, len(testImage)))))
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: testImage})))

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

func TestDescribePipeline(t *testing.T) {
	var (
		g       *gateway
		conn    *websocket.Conn
		getLogs func() string
	)

	setup := func(environment map[string]string) {
		getLogs = logger.InitTestLogger()
		g = newGateway(t, environment)
		conn = g.open(t)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(nil)
		id := uuid.New()

		all := describe(t, conn, id)

		// accepted, answer.start, then answer.text and the audio of every sentence, answer.end (PROTO-6)
		require.Equal(t, &protocol.Accepted{RequestID: id}, all[0].message)
		start := all[1].message.(*protocol.AnswerStart)
		require.Equal(t, &protocol.AnswerStart{
			RequestID: id,
			Kind:      protocol.AnswerKindDescription,
			Priority:  protocol.PriorityNormal,
			Audio:     &protocol.AudioFormat{Format: "pcm_s16le", SampleRate: servicestub.SampleRate, Channels: 1},
		}, start)
		texts := []string{}
		var frames uint32
		for i := 2; i < len(all)-1; i++ {
			got := all[i]
			if got.frame != nil {
				require.Equal(t, protocol.KindAudioOut, got.frame.Kind)
				require.Equal(t, id, got.frame.RequestID)
				require.Equal(t, frames, got.frame.Seq)
				require.Equal(t, servicestub.Marker(texts[len(texts)-1]), got.frame.Payload[0])
				frames++
				continue
			}
			text := got.message.(*protocol.AnswerText)
			require.Equal(t, len(texts), text.Seq)
			require.Equal(t, id, text.RequestID)
			// Every sentence has its audio right after its text.
			require.NotNil(t, all[i+1].frame)
			texts = append(texts, text.Text)
		}
		require.Equal(t, []string{"Впереди стол и два стула.", "Слева дверь, далеко.", "Справа шкаф, близко."}, texts)
		require.Equal(t, len(texts)*servicestub.ChunksPerSentence*servicestub.ChunkBytes, totalAudio(all))

		end := all[len(all)-1].message.(*protocol.AnswerEnd)
		require.Equal(t, id, end.RequestID)
		require.Equal(t, fakellm.DefaultText, end.Text)
		require.Equal(t, protocol.IntentDescribe, end.Intent)
		require.True(t, end.Hazard)
		require.False(t, end.Degraded)
		require.Equal(t, protocol.DegradedNone, end.DegradedReason)
		// Hazards first (PLT-2)
		require.Equal(t, []protocol.SceneObject{
			{Label: "stairs", LabelRU: "ступени", Direction: protocol.DirectionAhead, Distance: protocol.DistanceNear, Hazard: true},
			{Label: "chair", LabelRU: "стул", Direction: protocol.DirectionLeft, Distance: protocol.DistanceFar, Hazard: false},
		}, end.Objects)
		stages := make([]string, 0, len(end.Timings))
		for stage, value := range end.Timings {
			stages = append(stages, stage)
			require.GreaterOrEqual(t, float64(value), 0.0)
		}
		sort.Strings(stages)
		require.Equal(t, []string{
			pipeline.StageDetect, pipeline.StageLLMFirstSentence, pipeline.StageLLMTotal, pipeline.StageLLMTTFT,
			pipeline.StageQueue, pipeline.StageReceive, pipeline.StageServerTotal, pipeline.StageServerTTFA,
			pipeline.StageTTSFirstChunk, pipeline.StageTTSTotal,
		}, stages)
		require.LessOrEqual(t, end.Timings[pipeline.StageServerTTFA], end.Timings[pipeline.StageServerTotal])
		require.LessOrEqual(t, end.Timings[pipeline.StageLLMTTFT], end.Timings[pipeline.StageLLMFirstSentence])

		// The provider got the hazard first in the prompt and no identifiers (DSC-5, NFR-8)
		recorded := g.llm.Requests()
		require.Len(t, recorded, 1)
		body := string(recorded[0].Body)
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(recorded[0].Body, &request))
		user := request.Messages[1].Content
		require.Contains(t, user, "ступени - впереди, близко, опасность")
		require.Less(t, strings.Index(user, "ступени"), strings.Index(user, "стул"))
		require.NotContains(t, body, id.String())
		require.NotContains(t, body, "5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10")

		m := g.app.Metrics()
		require.Equal(t, 1.0, testutil.ToFloat64(m.Requests.WithLabelValues(pipeline.KindDescribe, pipeline.StatusOK)))
		require.Equal(t, len(stages), testutil.CollectAndCount(m.StageDuration))
		require.Contains(t, getLogs(), "INFO: request answered: request_id="+id.String()+" kind=describe status=ok sentences=3")
		// Texts of the answers go to the log only at DEBUG (PLT-20)
		for _, line := range strings.Split(getLogs(), "\n") {
			if strings.Contains(line, "стул") {
				require.True(t, strings.HasPrefix(line, "DEBUG: "), line)
			}
		}
	})

	t.Run("must_pass_template_hazard_first", func(t *testing.T) {
		setup(map[string]string{"APP_DESCRIBER_MODE": "template"})
		id := uuid.New()

		all := describe(t, conn, id)

		end := all[len(all)-1].message.(*protocol.AnswerEnd)
		require.True(t, strings.HasPrefix(end.Text, "Осторожно, впереди ступени."), end.Text)
		require.True(t, end.Hazard)
		require.NotContains(t, end.Timings, pipeline.StageLLMTTFT)
		require.Contains(t, end.Timings, pipeline.StageServerTTFA)
		require.Empty(t, g.llm.Requests())
	})

	t.Run("must_pass_focus_ahead", func(t *testing.T) {
		setup(map[string]string{"APP_DESCRIBER_MODE": "template"})
		id := uuid.New()
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
			`{"type":"describe","request_id":"%s","mode":"on_demand","focus":"ahead","image":{"width":640,"height":480,"bytes":13}}`, id))))
		require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: testImage})))

		var end *protocol.AnswerEnd
		for end == nil {
			if m, ok := read(t, conn).message.(*protocol.AnswerEnd); ok {
				end = m
			}
		}

		require.Equal(t, protocol.IntentDescribeAhead, end.Intent)
		require.Len(t, end.Objects, 1)
		require.Equal(t, "stairs", end.Objects[0].Label)
	})

	t.Run("must_fail_detector_unavailable", func(t *testing.T) {
		setup(map[string]string{"APP_DESCRIBER_MODE": "template"})
		g.detector.Close()
		id := uuid.New()

		all := describe(t, conn, id)

		require.Equal(t, &protocol.Accepted{RequestID: id}, all[0].message)
		require.Equal(t, &protocol.ErrorMessage{RequestID: &id, Code: protocol.CodeDetectorUnavailable, Message: "object detector unavailable", Retryable: true}, all[1].message)
	})
}

// totalAudio returns the number of audio bytes of the frames.
func totalAudio(all []received) int {
	total := 0
	for _, got := range all {
		if got.frame != nil {
			total += len(got.frame.Payload)
		}
	}
	return total
}

func TestOverheadBudget(t *testing.T) {
	const (
		warmup   = 20
		requests = 200
		budgetMS = 15.0
	)

	var (
		g    *gateway
		conn *websocket.Conn
	)

	setup := func() {
		logger.InitTestLogger()
		g = newGateway(t, map[string]string{"APP_LOG_LEVEL": "INFO"})
		conn = g.open(t)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()
		for i := 0; i < warmup; i++ {
			describe(t, conn, uuid.New())
		}

		totals := make([]float64, 0, requests)
		for i := 0; i < requests; i++ {
			all := describe(t, conn, uuid.New())
			end, ok := all[len(all)-1].message.(*protocol.AnswerEnd)
			require.True(t, ok)
			require.False(t, end.Degraded)
			totals = append(totals, float64(end.Timings[pipeline.StageServerTotal]))
		}
		p50 := stats.Percentile(totals, 0.5)
		p95 := stats.Percentile(totals, 0.95)
		t.Logf("server_total_ms with stubs without delay: requests=%d p50=%.1f p95=%.1f", requests, p50, p95)

		require.LessOrEqual(t, p95, budgetMS)
	})
}

func TestApp(t *testing.T) {
	var (
		g *gateway
	)

	setup := func() {
		logger.InitTestLogger()
		g = newGateway(t, nil)
	}

	t.Run("must_pass_welcome_config_id", func(t *testing.T) {
		setup()
		dialer := websocket.Dialer{Subprotocols: []string{session.Subprotocol}, HandshakeTimeout: testTimeout}
		conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(g.http.URL, "http")+session.Path, http.Header{"Authorization": {"Bearer " + testToken}})
		require.NoError(t, err)
		defer conn.Close()

		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(testHello)))
		welcome := read(t, conn).message.(*protocol.Welcome)

		require.Equal(t, Version, welcome.ServerVersion)
		require.Equal(t, g.app.collector.Current().ConfigID, welcome.ConfigID)
		require.Len(t, welcome.ConfigID, 12)
	})

	t.Run("must_pass_health", func(t *testing.T) {
		setup()

		response, err := http.Get(g.http.URL + "/health")

		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
	})

	t.Run("must_fail_not_ready", func(t *testing.T) {
		logger.InitTestLogger()
		pipelines := &readyPipelines{metrics: metrics.NewMetrics()}
		sink := &recordingSink{}
		id := uuid.New()

		pipelines.Describe(context.Background(), &pipeline.DescribeRequest{RequestID: id}, sink)

		require.Equal(t, []protocol.Message{&protocol.ErrorMessage{RequestID: &id, Code: protocol.CodeBusy, Message: errNotReady, Retryable: true}}, sink.messages)
	})
}

// recordingSink keeps the messages of a request.
type recordingSink struct {
	messages []protocol.Message
}

func (s *recordingSink) Send(message protocol.Message) {
	s.messages = append(s.messages, message)
}

func (s *recordingSink) SendAudio(uint32, []byte) {}
