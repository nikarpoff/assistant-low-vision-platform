package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// testAudio is a fake question: PCM s16le mono 16 kHz.
var testAudio = []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x00}

// askRun is one question: what the pipeline got and when.
type askRun struct {
	withImage bool
	// beforeEnd runs after ask.start and the snapshot, before ask.end.
	beforeEnd func()
}

// ask runs one question and waits for its end.
func (tp *testPipeline) ask(t *testing.T, run askRun) {
	t.Helper()
	image := make(chan []byte, 1)
	end := make(chan AskEnd, 1)
	if run.withImage {
		image <- testImage
	}
	close(image)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tp.pipeline.Ask(context.Background(), &AskRequest{
			RequestID: uuid.New(),
			SessionID: testSessionID,
			DeviceID:  testDeviceID,
			Trigger:   protocol.TriggerListenWindow,
			WithImage: run.withImage,
			StartedAt: time.Now(),
			Image:     image,
			End:       end,
		}, tp.sink)
	}()
	if run.beforeEnd != nil {
		run.beforeEnd()
	}
	end <- AskEnd{Audio: testAudio, AudioMS: 1830, ReceivedAt: time.Now()}
	wait(t, done, "ask")
}

// promptRecorder is a language model that keeps the user prompts and answers with the text.
type promptRecorder struct {
	mu      sync.Mutex
	answer  string
	prompts []string
}

func (c *promptRecorder) Complete(_ context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
	c.mu.Lock()
	for _, message := range req.Messages {
		if text, ok := message.Content.(string); ok && message.Role == llm.RoleUser {
			c.prompts = append(c.prompts, text)
		}
	}
	answer := c.answer
	c.mu.Unlock()
	return streamPieces(onText, answer)
}

func (c *promptRecorder) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

func TestQuestionPipeline(t *testing.T) {
	var (
		tp      *testPipeline
		model   *promptRecorder
		getLogs func() string
	)

	setup := func() {
		getLogs = logger.InitTestLogger()
		model = &promptRecorder{answer: "Дверь справа, далеко."}
		tp = newTestPipelineWith(t, pipelineOptions{completer: model})
	}

	door := detector.Object{Label: "door", LabelRU: "дверь", Confidence: 0.7, Direction: detector.DirectionRight, Distance: detector.DistanceFar, Area: 0.1}

	t.Run("must_pass", func(t *testing.T) {
		setup()
		tp.history.AddTurn(testDeviceID,
			history.Turn{Role: history.RoleQuestion, Text: "Что слева?"},
			history.Turn{Role: history.RoleAnswer, Text: "Слева стул, далеко."},
		)
		detectedBeforeEnd := false
		asrBeforeEnd := -1

		tp.ask(t, askRun{withImage: true, beforeEnd: func() {
			select {
			case <-tp.detector.Arrived():
				detectedBeforeEnd = true
			case <-time.After(testTimeout):
			}
			asrBeforeEnd = len(tp.asr.Bodies())
		}})

		// The detector ran on the snapshot while the user was still speaking
		require.True(t, detectedBeforeEnd)
		require.Equal(t, 0, asrBeforeEnd)
		require.Equal(t, 1, tp.detector.Requests())
		// The audio went to ASR in one body
		require.Equal(t, [][]byte{testAudio}, tp.asr.Bodies())

		messages := tp.sink.messages()
		transcript := messages[0].(*protocol.Transcript)
		require.Equal(t, "Где дверь?", transcript.Text)
		require.Equal(t, protocol.IntentQuestion, transcript.Intent)
		start := messages[1].(*protocol.AnswerStart)
		require.Equal(t, protocol.AnswerKindAnswer, start.Kind)
		require.Equal(t, protocol.PriorityNormal, start.Priority)
		require.NotNil(t, start.Audio)
		require.Equal(t, []string{"Дверь справа, далеко."}, tp.sink.texts())
		require.Positive(t, tp.sink.frames())
		end := tp.sink.last(t).(*protocol.AnswerEnd)
		require.Equal(t, "Дверь справа, далеко.", end.Text)
		require.Equal(t, protocol.IntentQuestion, end.Intent)
		require.False(t, end.Degraded)
		require.Contains(t, end.Timings, StageASR)
		require.Contains(t, end.Timings, StageDetect)

		// The prompt has the question, the objects of the snapshot and the history
		prompts := model.calls()
		require.Len(t, prompts, 1)
		require.Contains(t, prompts[0], "Где дверь?")
		require.Contains(t, prompts[0], "Слева стул, далеко.")
		require.Contains(t, prompts[0], "ступени")

		// The history got the scene and the turns
		h := tp.history.Get(testDeviceID)
		require.Len(t, h.Scenes, 1)
		require.Equal(t, tp.detector.Objects(), h.Scenes[0].Objects)
		require.Equal(t, "Дверь справа, далеко.", h.Scenes[0].Description)
		require.Len(t, h.Turns, 4)
		require.Equal(t, history.Turn{Role: history.RoleQuestion, Text: "Где дверь?", At: h.Turns[2].At}, h.Turns[2])
		require.Equal(t, history.Turn{Role: history.RoleAnswer, Text: "Дверь справа, далеко.", At: h.Turns[3].At}, h.Turns[3])

		record := tp.lastRecord(t)
		require.Equal(t, KindAsk, record.Kind)
		require.Equal(t, StatusOK, record.Status)
		require.Equal(t, "Где дверь?", record.Transcript)
		require.Equal(t, string(protocol.IntentQuestion), record.Intent)
		require.Equal(t, len(testImage), record.ImageBytes)
		require.NotContains(t, getLogs(), "ERROR:")
	})

	t.Run("must_pass_without_image_fresh_scene", func(t *testing.T) {
		setup()
		tp.history.AddScene(testDeviceID, history.Scene{Objects: []detector.Object{door}, Description: "Справа дверь, далеко.", At: time.Now().Add(-time.Second)})

		tp.ask(t, askRun{})

		require.Equal(t, 0, tp.detector.Requests())
		end := tp.sink.last(t).(*protocol.AnswerEnd)
		require.Equal(t, "Дверь справа, далеко.", end.Text)
		require.Equal(t, []protocol.SceneObject{{Label: "door", LabelRU: "дверь", Direction: protocol.Direction("right"), Distance: protocol.Distance("far")}}, end.Objects)
		require.NotContains(t, end.Timings, StageDetect)
		prompts := model.calls()
		require.Len(t, prompts, 1)
		require.Contains(t, prompts[0], "Объекты текущей сцены: дверь")
		// No new scene: the question had no snapshot
		h := tp.history.Get(testDeviceID)
		require.Len(t, h.Scenes, 1)
		require.Len(t, h.Turns, 2)
	})

	t.Run("must_pass_without_image_stale_scene", func(t *testing.T) {
		setup()
		tp.history.AddScene(testDeviceID, history.Scene{Objects: []detector.Object{door}, Description: "Справа дверь, далеко.", At: time.Now().Add(-11 * time.Second)})

		tp.ask(t, askRun{})

		require.Equal(t, 0, tp.detector.Requests())
		end := tp.sink.last(t).(*protocol.AnswerEnd)
		require.Equal(t, "Дверь справа, далеко.", end.Text)
		require.Empty(t, end.Objects)
		prompts := model.calls()
		require.Len(t, prompts, 1)
		// The answer is built by the history alone
		require.Contains(t, prompts[0], "Объекты текущей сцены: ничего не найдено")
		require.Contains(t, prompts[0], "Предыдущие сцены")
		require.Contains(t, prompts[0], "Где дверь?")
	})

	t.Run("must_pass_app_command", func(t *testing.T) {
		setup()
		tp.asr.SetText("Повтори, пожалуйста.")

		tp.ask(t, askRun{withImage: true})

		messages := tp.sink.messages()
		require.Len(t, messages, 2)
		transcript := messages[0].(*protocol.Transcript)
		require.Equal(t, protocol.IntentRepeat, transcript.Intent)
		end := messages[1].(*protocol.AnswerEnd)
		require.Equal(t, "", end.Text)
		require.Equal(t, protocol.IntentRepeat, end.Intent)
		require.Equal(t, []protocol.SceneObject{}, end.Objects)
		require.Equal(t, 0, tp.sink.frames())
		require.Empty(t, model.calls())
		require.Empty(t, tp.tts.Texts())
		require.Empty(t, tp.history.Get(testDeviceID).Turns)
		require.Equal(t, StatusOK, tp.lastRecord(t).Status)
	})

	t.Run("must_pass_describe_ahead", func(t *testing.T) {
		setup()
		tp.asr.SetText("Что впереди?")
		model.answer = "Осторожно, впереди близко ступени."

		tp.ask(t, askRun{withImage: true})

		transcript := find[*protocol.Transcript](t, tp.sink)
		require.Equal(t, protocol.IntentDescribeAhead, transcript.Intent)
		start := find[*protocol.AnswerStart](t, tp.sink)
		require.Equal(t, protocol.AnswerKindDescription, start.Kind)
		end := tp.sink.last(t).(*protocol.AnswerEnd)
		require.Equal(t, protocol.IntentDescribeAhead, end.Intent)
		require.True(t, end.Hazard)
		// Only the objects ahead: the chair on the left is out of focus
		require.Equal(t, []protocol.SceneObject{{Label: "stairs", LabelRU: "ступени", Direction: "ahead", Distance: "near", Hazard: true}}, end.Objects)
		prompts := model.calls()
		require.Len(t, prompts, 1)
		require.Contains(t, prompts[0], "Фокус: только то, что впереди.")
		require.Contains(t, prompts[0], "Вопрос: Опиши обстановку")
	})

	t.Run("must_fail_asr_empty", func(t *testing.T) {
		setup()
		tp.asr.SetText("")

		tp.ask(t, askRun{withImage: true})

		messages := tp.sink.messages()
		require.Len(t, messages, 1)
		require.Equal(t, protocol.NewErrorMessage(messages[0].(*protocol.ErrorMessage).RequestID, protocol.NewError(protocol.CodeASREmpty, errASREmpty)), messages[0])
		require.True(t, messages[0].(*protocol.ErrorMessage).Retryable)
		require.Empty(t, model.calls())
		require.Equal(t, StatusError, tp.lastRecord(t).Status)
		require.Equal(t, string(protocol.CodeASREmpty), tp.lastRecord(t).ErrorCode)
	})

	t.Run("must_fail_asr_unavailable", func(t *testing.T) {
		setup()
		tp.asr.Fail(true)

		tp.ask(t, askRun{withImage: true})

		messages := tp.sink.messages()
		require.Len(t, messages, 1)
		require.Equal(t, protocol.NewErrorMessage(messages[0].(*protocol.ErrorMessage).RequestID, protocol.NewError(protocol.CodeASRUnavailable, errASRUnavailable)), messages[0])
		require.Empty(t, model.calls())
		require.Equal(t, string(protocol.CodeASRUnavailable), tp.lastRecord(t).ErrorCode)
	})
}
