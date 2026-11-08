package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/journal"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/servicestub"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/tts"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Settings of the tests.
const (
	testToken         = "device-token-1"
	testConfigID      = "c9f2a1d0e3b4"
	testVersion       = "0.1.0"
	testMaxImageBytes = 64 << 10
	testTimeout       = 5 * time.Second
	testHello         = `{"type":"hello","protocol":1,"device_id":"5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10","app_version":"0.1.0","locale":"ru-RU","platform":"android-34","network":"wifi"}`
)

// testImage is a small fake JPEG snapshot.
var testImage = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}

// discardRecorder drops the journal records.
type discardRecorder struct{}

func (discardRecorder) Write(journal.Record) {}

// harness is a session server over a real pipeline in the template mode and the service stubs.
type harness struct {
	server   *Server
	http     *httptest.Server
	detector *servicestub.Detector
	tts      *servicestub.TTS
	metrics  *metrics.Metrics
	cfg      Config
}

func newHarness(t *testing.T, mutate func(cfg *Config)) *harness {
	t.Helper()
	h := &harness{detector: servicestub.NewDetector(), tts: servicestub.NewTTS(), metrics: metrics.NewMetrics()}
	t.Cleanup(h.detector.Close)
	t.Cleanup(h.tts.Close)

	vars := &env.Vars{
		AppDeviceTokens:           []string{"other-token", testToken},
		AppDescriberMode:          env.DescriberModeTemplate,
		AppDetectorURL:            h.detector.URL,
		AppDetectTimeoutMS:        2000,
		AppTTSURL:                 h.tts.URL,
		AppTTSFirstChunkTimeoutMS: 2000,
		AppMaxAnswerChars:         220,
		AppRequestDeadlineMS:      8000,
		AppImageMaxSide:           640,
		AppJPEGQuality:            80,
		AppPassiveIntervalMS:      3000,
		AppListenWindowMS:         5000,
		AppMaxQuestionMS:          15000,
		AppAudioOutSampleRate:     24000,
		AppMaxImageBytes:          testMaxImageBytes,
	}
	detectorClient, err := detector.NewClient(vars, h.metrics)
	require.NoError(t, err)
	ttsClient, err := tts.NewClient(vars, h.metrics)
	require.NoError(t, err)

	body, err := os.ReadFile(servicestub.ContractPath("objects-detector", "vocabulary.response.json"))
	require.NoError(t, err)
	document := &vocabulary.Document{}
	require.NoError(t, json.Unmarshal(body, document))
	vocab, err := vocabulary.NewVocabulary(document)
	require.NoError(t, err)
	rules, err := hazard.LoadRules("")
	require.NoError(t, err)
	policy, err := hazard.NewPolicy(rules, vocab)
	require.NoError(t, err)
	sceneDescriber, err := describer.NewDescriber(describer.NewConfig(vars), vocab, policy, nil, nil)
	require.NoError(t, err)
	store, err := history.NewStore(5, 6, time.Minute, time.Now)
	require.NoError(t, err)
	configSource := &ConfigSourceMock{CurrentFunc: func() *snapshot.Snapshot {
		return &snapshot.Snapshot{ConfigID: testConfigID}
	}}

	pipelines := pipeline.NewPipeline(pipeline.NewConfig(vars), pipeline.Dependencies{
		Detector:    detectorClient,
		Synthesizer: ttsClient,
		Describer:   sceneDescriber,
		History:     store,
		Policy:      policy,
		Recorder:    discardRecorder{},
		Config:      configSource,
		Metrics:     h.metrics,
	})

	h.cfg = NewConfig(vars, testVersion)
	if mutate != nil {
		mutate(&h.cfg)
	}
	h.server = NewServer(h.cfg, pipelines, configSource, h.metrics)
	h.http = httptest.NewServer(h.server)
	t.Cleanup(h.http.Close)
	return h
}

// dial opens a session; token is empty for no Authorization header.
func (h *harness) dial(token string, subprotocols ...string) (*client, *http.Response, error) {
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialer := websocket.Dialer{Subprotocols: subprotocols, EnableCompression: true, HandshakeTimeout: testTimeout}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(h.http.URL, "http")+Path, header)
	if err != nil {
		return nil, response, err
	}
	return &client{conn: conn}, response, nil
}

// open opens a session with the device token and says hello.
func (h *harness) open(t *testing.T) *client {
	t.Helper()
	c, _, err := h.dial(testToken, Subprotocol)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.conn.Close() })
	c.send(t, testHello)
	require.IsType(t, &protocol.Welcome{}, c.next(t).message)
	return c
}

// client is the application side of a session.
type client struct {
	conn *websocket.Conn
}

// received is a message or a frame from the gateway.
type received struct {
	message protocol.Message
	frame   *protocol.Frame
}

func (c *client) send(t *testing.T, message string) {
	t.Helper()
	require.NoError(t, c.conn.WriteMessage(websocket.TextMessage, []byte(message)))
}

func (c *client) sendFrame(t *testing.T, data []byte) {
	t.Helper()
	require.NoError(t, c.conn.WriteMessage(websocket.BinaryMessage, data))
}

// describe sends describe and its snapshot.
func (c *client) describe(t *testing.T, id uuid.UUID, image []byte) {
	t.Helper()
	c.send(t, fmt.Sprintf(`{"type":"describe","request_id":"%s","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":%d}}`, id, len(image)))
	c.sendFrame(t, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: image}))
}

// next reads the next message or frame from the gateway.
func (c *client) next(t *testing.T) received {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	messageType, data, err := c.conn.ReadMessage()
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

// nextError reads the next message and requires an error.
func (c *client) nextError(t *testing.T) *protocol.ErrorMessage {
	t.Helper()
	got := c.next(t)
	require.IsType(t, &protocol.ErrorMessage{}, got.message)
	return got.message.(*protocol.ErrorMessage)
}

// until reads until answer.end or error of the request and returns everything read.
func (c *client) until(t *testing.T, id uuid.UUID) []received {
	t.Helper()
	all := []received{}
	for {
		got := c.next(t)
		all = append(all, got)
		switch m := got.message.(type) {
		case *protocol.AnswerEnd:
			if m.RequestID == id {
				return all
			}
		case *protocol.ErrorMessage:
			if m.RequestID != nil && *m.RequestID == id {
				return all
			}
		}
	}
}

// requireOpen proves the session is open: a describe request gets accepted and answer.end.
func (c *client) requireOpen(t *testing.T) {
	t.Helper()
	id := uuid.New()
	c.describe(t, id, testImage)
	all := c.until(t, id)
	require.Equal(t, &protocol.Accepted{RequestID: id}, all[0].message)
	require.IsType(t, &protocol.AnswerEnd{}, all[len(all)-1].message)
}

func TestSession_Handshake(t *testing.T) {
	var (
		h       *harness
		getLogs func() string
	)

	setup := func(mutate func(cfg *Config)) {
		getLogs = logger.InitTestLogger()
		h = newHarness(t, mutate)
	}

	t.Run("must_pass_welcome", func(t *testing.T) {
		setup(nil)

		c, response, err := h.dial(testToken, Subprotocol)
		require.NoError(t, err)
		defer c.conn.Close()
		c.send(t, testHello)
		got := c.next(t)

		require.Equal(t, Subprotocol, response.Header.Get("Sec-WebSocket-Protocol"))
		require.Empty(t, response.Header.Get("Sec-WebSocket-Extensions"))
		welcome := got.message.(*protocol.Welcome)
		require.NotEqual(t, uuid.Nil, welcome.SessionID)
		require.Equal(t, testVersion, welcome.ServerVersion)
		require.Equal(t, testConfigID, welcome.ConfigID)
		require.Equal(t, protocol.ClientConfig{
			ImageMaxSide:      640,
			JPEGQuality:       80,
			PassiveIntervalMS: 3000,
			ListenWindowMS:    5000,
			MaxQuestionMS:     15000,
			AudioIn:           protocol.AudioFormat{Format: "pcm_s16le", SampleRate: 16000, Channels: 1},
			AudioOut:          protocol.AudioFormat{Format: "pcm_s16le", SampleRate: 24000, Channels: 1},
		}, welcome.Config)
		require.Equal(t, 1.0, testutil.ToFloat64(h.metrics.SessionsActive))
		require.Equal(t, 1, h.server.Sessions())
		require.Contains(t, getLogs(), "INFO: Session hello: session_id="+welcome.SessionID.String())
		require.NotContains(t, getLogs(), testToken)

		require.NoError(t, c.conn.Close())
		require.Eventually(t, func() bool { return h.server.Sessions() == 0 }, testTimeout, 10*time.Millisecond)
		require.Equal(t, 0.0, testutil.ToFloat64(h.metrics.SessionsActive))
	})

	t.Run("must_pass_auth_disabled", func(t *testing.T) {
		setup(func(cfg *Config) { cfg.AuthDisabled = true })

		c, response, err := h.dial("", Subprotocol)
		require.NoError(t, err)
		defer c.conn.Close()
		c.send(t, testHello)
		got := c.next(t)

		require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		require.Equal(t, testConfigID, got.message.(*protocol.Welcome).ConfigID)
	})

	t.Run("must_fail_no_token", func(t *testing.T) {
		setup(nil)

		_, response, err := h.dial("", Subprotocol)

		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		body, _ := io.ReadAll(response.Body)
		require.JSONEq(t, `{"error":"unauthorized"}`, string(body))
		require.Equal(t, 0, h.server.Sessions())
	})

	t.Run("must_fail_bad_token", func(t *testing.T) {
		setup(nil)

		_, response, err := h.dial("wrong-token", Subprotocol)

		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		require.Contains(t, getLogs(), "WARNING: unauthorized session request: remote=")
		require.NotContains(t, getLogs(), "wrong-token")
	})

	t.Run("must_fail_no_subprotocol", func(t *testing.T) {
		setup(nil)

		_, response, err := h.dial(testToken, "alv.v2")

		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		body, _ := io.ReadAll(response.Body)
		require.JSONEq(t, `{"error":"`+msgSubprotocolRequired+`"}`, string(body))
	})

	t.Run("must_fail_unsupported_protocol", func(t *testing.T) {
		setup(nil)
		c, _, err := h.dial(testToken, Subprotocol)
		require.NoError(t, err)
		defer c.conn.Close()

		c.send(t, strings.Replace(testHello, `"protocol":1`, `"protocol":2`, 1))
		errorMessage := c.nextError(t)
		_, _, readErr := c.conn.ReadMessage()

		require.Equal(t, &protocol.ErrorMessage{Code: protocol.CodeUnsupportedProtocol, Message: "unsupported protocol version 2", Retryable: false}, errorMessage)
		var closeErr *websocket.CloseError
		require.True(t, errors.As(readErr, &closeErr), readErr)
		require.Equal(t, CloseUnsupportedProtocol, closeErr.Code)
	})
}

func TestSession_BadInput(t *testing.T) {
	var (
		h *harness
		c *client
	)

	setup := func() {
		logger.InitTestLogger()
		h = newHarness(t, nil)
		c = h.open(t)
	}

	t.Run("must_pass_short_frame", func(t *testing.T) {
		setup()

		c.sendFrame(t, []byte{0x01, 0x01, 0x12, 0x3e})
		errorMessage := c.nextError(t)

		require.Nil(t, errorMessage.RequestID)
		require.Equal(t, protocol.CodeBadRequest, errorMessage.Code)
		require.Equal(t, "frame is shorter than the 22-byte header: 4 bytes", errorMessage.Message)
		require.False(t, errorMessage.Retryable)
		c.requireOpen(t)
	})

	t.Run("must_pass_unknown_kind", func(t *testing.T) {
		setup()
		data := protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: uuid.New(), Payload: testImage})
		data[1] = 0x05

		c.sendFrame(t, data)
		errorMessage := c.nextError(t)

		require.Equal(t, protocol.CodeBadRequest, errorMessage.Code)
		require.Equal(t, "unknown frame kind 0x05", errorMessage.Message)
		c.requireOpen(t)
	})

	t.Run("must_pass_unknown_version", func(t *testing.T) {
		setup()
		data := protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: uuid.New(), Payload: testImage})
		data[0] = 0x02

		c.sendFrame(t, data)
		errorMessage := c.nextError(t)

		require.Equal(t, protocol.CodeBadRequest, errorMessage.Code)
		require.Equal(t, "unknown frame version 0x02", errorMessage.Message)
		c.requireOpen(t)
	})

	t.Run("must_pass_message_before_hello", func(t *testing.T) {
		logger.InitTestLogger()
		h = newHarness(t, nil)
		var err error
		c, _, err = h.dial(testToken, Subprotocol)
		require.NoError(t, err)
		defer c.conn.Close()

		c.send(t, `{"type":"cancel","request_id":"123e4567-e89b-12d3-a456-426614174000"}`)
		messageError := c.nextError(t)
		c.sendFrame(t, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: uuid.New(), Payload: testImage}))
		frameError := c.nextError(t)
		c.send(t, testHello)

		require.Equal(t, &protocol.ErrorMessage{Code: protocol.CodeBadRequest, Message: errHelloExpected}, messageError)
		require.Equal(t, &protocol.ErrorMessage{Code: protocol.CodeBadRequest, Message: errFrameBeforeHello}, frameError)
		require.IsType(t, &protocol.Welcome{}, c.next(t).message)
		c.requireOpen(t)
	})

	t.Run("must_pass_unknown_type", func(t *testing.T) {
		setup()

		c.send(t, `{"type":"describe.all","request_id":"123e4567-e89b-12d3-a456-426614174000"}`)
		errorMessage := c.nextError(t)

		require.Equal(t, &protocol.ErrorMessage{Code: protocol.CodeBadRequest, Message: `unknown message type "describe.all"`}, errorMessage)
		c.requireOpen(t)
	})

	t.Run("must_pass_payload_too_large", func(t *testing.T) {
		setup()
		id := uuid.New()

		c.describe(t, id, make([]byte, testMaxImageBytes+1))
		errorMessage := c.nextError(t)

		require.Equal(t, &id, errorMessage.RequestID)
		require.Equal(t, protocol.CodePayloadTooLarge, errorMessage.Code)
		require.Equal(t, fmt.Sprintf("image exceeds %d bytes", testMaxImageBytes), errorMessage.Message)
		require.Zero(t, h.detector.Requests())
		require.Equal(t, 1.0, testutil.ToFloat64(h.metrics.Requests.WithLabelValues(pipeline.KindDescribe, pipeline.StatusError)))
		c.requireOpen(t)
	})

	t.Run("must_pass_question_too_long", func(t *testing.T) {
		setup()
		id := uuid.New()
		c.send(t, fmt.Sprintf(`{"type":"ask.start","request_id":"%s","trigger":"hold","with_image":false}`, id))

		c.send(t, fmt.Sprintf(`{"type":"ask.end","request_id":"%s","audio_ms":15001}`, id))
		errorMessage := c.nextError(t)

		require.Equal(t, &id, errorMessage.RequestID)
		require.Equal(t, protocol.CodePayloadTooLarge, errorMessage.Code)
		require.Equal(t, "question exceeds 15000 ms", errorMessage.Message)
		c.requireOpen(t)
	})

	t.Run("must_pass_audio_out_from_client", func(t *testing.T) {
		setup()
		id := uuid.New()

		c.sendFrame(t, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindAudioOut, RequestID: id, Payload: []byte{0, 0}}))
		errorMessage := c.nextError(t)

		require.Equal(t, &protocol.ErrorMessage{RequestID: &id, Code: protocol.CodeBadRequest, Message: errAudioOutFromClient}, errorMessage)
		c.requireOpen(t)
	})

	t.Run("must_pass_unexpected_image", func(t *testing.T) {
		setup()
		id := uuid.New()

		c.sendFrame(t, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: testImage}))
		errorMessage := c.nextError(t)

		require.Equal(t, &protocol.ErrorMessage{RequestID: &id, Code: protocol.CodeBadRequest, Message: errUnexpectedImage}, errorMessage)
		c.requireOpen(t)
	})
}

func TestSession_Cancel(t *testing.T) {
	var (
		h *harness
		c *client
	)

	setup := func() {
		logger.InitTestLogger()
		h = newHarness(t, nil)
		c = h.open(t)
	}

	cancelled := func() float64 {
		return testutil.ToFloat64(h.metrics.Requests.WithLabelValues(pipeline.KindDescribe, pipeline.StatusCancelled))
	}

	t.Run("must_pass_cancel_message", func(t *testing.T) {
		setup()
		h.detector.Hold(true)
		id := uuid.New()
		c.describe(t, id, testImage)
		require.Equal(t, &protocol.Accepted{RequestID: id}, c.next(t).message)
		select {
		case <-h.detector.Arrived():
		case <-time.After(testTimeout):
			t.Fatal("detector was not called")
		}

		c.send(t, fmt.Sprintf(`{"type":"cancel","request_id":"%s"}`, id))

		require.Eventually(t, func() bool { return h.detector.Cancelled() == 1 }, testTimeout, 5*time.Millisecond)
		require.Eventually(t, func() bool { return cancelled() == 1 }, testTimeout, 5*time.Millisecond)
		// The pipeline is over: the next message is the answer to the probe, nothing of the request came.
		c.send(t, testHello)
		require.Equal(t, &protocol.ErrorMessage{Code: protocol.CodeBadRequest, Message: errHelloRepeated}, c.nextError(t))
		require.Zero(t, h.tts.Texts())
	})

	t.Run("must_pass_new_request_cancels_previous", func(t *testing.T) {
		setup()
		h.tts.HoldFirst(true)
		first := uuid.New()
		c.describe(t, first, testImage)
		for got := c.next(t); got.frame == nil; got = c.next(t) {
			require.Equal(t, first, requestIDOf(got))
		}

		second := uuid.New()
		c.describe(t, second, testImage)
		all := c.until(t, second)

		accepted := -1
		for i, got := range all {
			if got.message != nil {
				if m, ok := got.message.(*protocol.Accepted); ok && m.RequestID == second {
					accepted = i
				}
			}
		}
		require.GreaterOrEqual(t, accepted, 0)
		var frames uint32
		for _, got := range all[accepted:] {
			require.Equal(t, second, requestIDOf(got))
			if got.frame != nil {
				require.Equal(t, frames, got.frame.Seq)
				frames++
			}
		}
		require.Positive(t, frames)
		end := all[len(all)-1].message.(*protocol.AnswerEnd)
		require.False(t, end.Degraded)
		require.Eventually(t, func() bool { return h.tts.Cancelled() >= 1 }, testTimeout, 5*time.Millisecond)
		require.Eventually(t, func() bool { return cancelled() == 1 }, testTimeout, 5*time.Millisecond)
		require.Equal(t, 1.0, testutil.ToFloat64(h.metrics.Requests.WithLabelValues(pipeline.KindDescribe, pipeline.StatusOK)))
	})

	t.Run("must_pass_cancel_before_snapshot", func(t *testing.T) {
		setup()
		id := uuid.New()
		c.send(t, fmt.Sprintf(`{"type":"describe","request_id":"%s","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":13}}`, id))

		c.send(t, fmt.Sprintf(`{"type":"cancel","request_id":"%s"}`, id))
		c.sendFrame(t, protocol.EncodeFrame(&protocol.Frame{Kind: protocol.KindImageJPEG, RequestID: id, Payload: testImage}))
		errorMessage := c.nextError(t)

		require.Equal(t, &protocol.ErrorMessage{RequestID: &id, Code: protocol.CodeBadRequest, Message: errUnexpectedImage}, errorMessage)
		require.Equal(t, 1.0, cancelled())
		require.Zero(t, h.detector.Requests())
	})
}

func TestSession_Keepalive(t *testing.T) {
	var (
		h     *harness
		c     *client
		pings atomic.Int32
		read  chan error
	)

	setup := func(pong bool) {
		logger.InitTestLogger()
		h = newHarness(t, func(cfg *Config) {
			cfg.PingInterval = 20 * time.Millisecond
			cfg.PongTimeout = 150 * time.Millisecond
		})
		var err error
		c, _, err = h.dial(testToken, Subprotocol)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.conn.Close() })
		pings.Store(0)
		c.conn.SetPingHandler(func(data string) error {
			pings.Add(1)
			if !pong {
				return nil
			}
			return c.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
		})
		read = make(chan error, 1)
		go func() {
			for {
				if _, _, err := c.conn.ReadMessage(); err != nil {
					read <- err
					return
				}
			}
		}()
	}

	t.Run("must_pass_ping_sent", func(t *testing.T) {
		setup(true)

		// Several pong timeouts pass: the session stays open while pongs come.
		time.Sleep(500 * time.Millisecond)

		require.GreaterOrEqual(t, pings.Load(), int32(5))
		require.Equal(t, 1, h.server.Sessions())
		select {
		case err := <-read:
			t.Fatalf("session closed: %v", err)
		default:
		}
	})

	t.Run("must_pass_closed_without_pong", func(t *testing.T) {
		setup(false)

		var err error
		select {
		case err = <-read:
		case <-time.After(testTimeout):
			t.Fatal("session was not closed")
		}

		require.Error(t, err)
		require.Positive(t, pings.Load())
		require.Eventually(t, func() bool { return h.server.Sessions() == 0 }, testTimeout, 10*time.Millisecond)
	})

	t.Run("must_pass_defaults", func(t *testing.T) {
		cfg := NewConfig(&env.Vars{AppMaxImageBytes: 2097152}, testVersion)

		require.Equal(t, 15*time.Second, cfg.PingInterval)
		require.Equal(t, 30*time.Second, cfg.PongTimeout)
		require.Equal(t, 5*time.Second, cfg.ShutdownGrace)
		require.Equal(t, DefaultPingInterval, cfg.PingInterval)
		require.Equal(t, DefaultPongTimeout, cfg.PongTimeout)
	})
}

func TestSession_Shutdown(t *testing.T) {
	var (
		h *harness
		c *client
	)

	setup := func() {
		logger.InitTestLogger()
		h = newHarness(t, func(cfg *Config) { cfg.ShutdownGrace = 2 * time.Second })
		c = h.open(t)
	}

	t.Run("must_pass_close_going_away", func(t *testing.T) {
		setup()
		h.tts.ChunkPause(100 * time.Millisecond)
		id := uuid.New()
		c.describe(t, id, testImage)
		require.Equal(t, &protocol.Accepted{RequestID: id}, c.next(t).message)
		done := make(chan struct{})

		go func() {
			defer close(done)
			h.server.Shutdown(context.Background())
		}()
		all := c.until(t, id)
		_, _, err := c.conn.ReadMessage()
		<-done

		// The active request finished before the session was closed.
		require.IsType(t, &protocol.AnswerEnd{}, all[len(all)-1].message)
		var closeErr *websocket.CloseError
		require.True(t, errors.As(err, &closeErr), err)
		require.Equal(t, websocket.CloseGoingAway, closeErr.Code)
		_, response, err := h.dial(testToken, Subprotocol)
		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	})
}

// requestIDOf returns the request_id of a message or a frame of a request.
func requestIDOf(got received) uuid.UUID {
	if got.frame != nil {
		return got.frame.RequestID
	}
	switch m := got.message.(type) {
	case *protocol.Accepted:
		return m.RequestID
	case *protocol.AnswerStart:
		return m.RequestID
	case *protocol.AnswerText:
		return m.RequestID
	case *protocol.AnswerEnd:
		return m.RequestID
	case *protocol.ErrorMessage:
		if m.RequestID != nil {
			return *m.RequestID
		}
	}
	return uuid.Nil
}
