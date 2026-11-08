package pipeline

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// fakeFirstSentence is the first sentence of fakellm.DefaultText.
const fakeFirstSentence = "Впереди стол и два стула."

// startFakeLLM starts the language model stub and returns it with its base URL (APP_LLM_URL).
func startFakeLLM(t *testing.T, cfg fakellm.Config) (*fakellm.Server, string) {
	t.Helper()
	stub := fakellm.NewServer(cfg)
	server := httptest.NewServer(stub)
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return stub, server.URL + fakellm.BasePath
}

func TestDegradation(t *testing.T) {
	var (
		tp      *testPipeline
		fake    *fakellm.Server
		getLogs func() string
	)

	setup := func(cfg fakellm.Config, opts pipelineOptions) {
		getLogs = logger.InitTestLogger()
		fake, opts.llmURL = startFakeLLM(t, cfg)
		tp = newTestPipelineWith(t, opts)
	}

	// templateText is the template description of the objects of the detector.
	templateText := func(t *testing.T) string {
		t.Helper()
		result, err := tp.describer.Template(&describer.Input{Objects: tp.detector.Objects(), Focus: protocol.FocusAll}, nil)
		require.NoError(t, err)
		return result.Text
	}

	// requireAnswer checks the answer: no error, answer.end with the text and the degraded reason.
	requireAnswer := func(t *testing.T, text string, reason protocol.DegradedReason) *protocol.AnswerEnd {
		t.Helper()
		for _, message := range tp.sink.messages() {
			require.NotEqual(t, protocol.TypeError, message.MessageType())
		}
		end := tp.sink.last(t).(*protocol.AnswerEnd)
		require.Equal(t, text, end.Text)
		require.Equal(t, strings.Join(tp.sink.texts(), " "), end.Text)
		require.Equal(t, reason != protocol.DegradedNone, end.Degraded)
		require.Equal(t, reason, end.DegradedReason)
		return end
	}

	// requireError checks that the request ended with the error of the code and nothing else.
	requireError := func(t *testing.T, code protocol.ErrorCode) {
		t.Helper()
		messages := tp.sink.messages()
		require.Len(t, messages, 1)
		message := messages[0].(*protocol.ErrorMessage)
		require.Equal(t, code, message.Code)
		require.True(t, message.Retryable)
		require.Equal(t, 0, tp.sink.frames())
		require.Equal(t, StatusError, tp.lastRecord(t).Status)
		require.Equal(t, string(code), tp.lastRecord(t).ErrorCode)
	}

	t.Run("must_pass_llm_timeout", func(t *testing.T) {
		setup(fakellm.Config{FirstTokenDelayMS: 3000}, pipelineOptions{llmTTFTMS: 100})

		started := time.Now()
		tp.describe(t)

		require.Less(t, time.Since(started), 2*time.Second)
		end := requireAnswer(t, templateText(t), protocol.DegradedLLMTimeout)
		require.True(t, end.Hazard)
		require.NotNil(t, find[*protocol.AnswerStart](t, tp.sink).Audio)
		require.Positive(t, tp.sink.frames())
		require.NotContains(t, end.Timings, StageLLMFirstSentence)
		record := tp.lastRecord(t)
		require.Equal(t, StatusDegraded, record.Status)
		require.Equal(t, string(llm.ReasonTimeoutTTFT), record.LLMError)
		require.Equal(t, string(protocol.DegradedLLMTimeout), record.DegradedReason)
		require.Len(t, fake.Requests(), 1)
	})

	t.Run("must_pass_llm_total_timeout_after_sentence", func(t *testing.T) {
		setup(fakellm.Config{
			Text:         fakeFirstSentence + " Слева дверь, далеко, и ещё много слов про эту комнату и её вещи.",
			TokenDelayMS: 100,
		}, pipelineOptions{llmTotalMS: 900})

		tp.describe(t)

		// The answer is closed with the ready sentence, the tail is dropped
		end := requireAnswer(t, fakeFirstSentence, protocol.DegradedLLMTimeout)
		require.Contains(t, end.Timings, StageLLMFirstSentence)
		require.Equal(t, []string{fakeFirstSentence}, tp.tts.Texts())
		require.Equal(t, string(llm.ReasonTimeoutTotal), tp.lastRecord(t).LLMError)
		require.Len(t, fake.Requests(), 1)
	})

	t.Run("must_pass_tts_unavailable", func(t *testing.T) {
		setup(fakellm.Config{}, pipelineOptions{})
		tp.tts.Fail(true)

		tp.describe(t)

		start := tp.sink.messages()[0].(*protocol.AnswerStart)
		require.Nil(t, start.Audio)
		require.Equal(t, []string{fakeFirstSentence, "Слева дверь, далеко.", "Справа шкаф, близко."}, tp.sink.texts())
		require.Equal(t, 0, tp.sink.frames())
		requireAnswer(t, fakellm.DefaultText, protocol.DegradedTTSUnavailable)
		require.Equal(t, StatusDegraded, tp.lastRecord(t).Status)
	})

	t.Run("must_fail_detector_unavailable", func(t *testing.T) {
		setup(fakellm.Config{}, pipelineOptions{})
		tp.detector.Close()

		tp.describe(t)

		requireError(t, protocol.CodeDetectorUnavailable)
		require.Empty(t, fake.Requests())
	})

	t.Run("must_pass_vlm_without_detector", func(t *testing.T) {
		setup(fakellm.Config{}, pipelineOptions{mode: env.DescriberModeVLM})
		tp.detector.Close()

		tp.describe(t)

		end := requireAnswer(t, fakellm.DefaultText, protocol.DegradedNone)
		require.Empty(t, end.Objects)
		requests := fake.Requests()
		require.Len(t, requests, 1)
		// The snapshot goes to the model without hints of the detector
		require.Contains(t, string(requests[0].Body), "data:image/jpeg;base64,")
		require.NotContains(t, string(requests[0].Body), "Подсказки детектора")
		require.Contains(t, getLogs(), "the vlm mode describes the snapshot alone")
	})

	t.Run("must_pass_llm_unavailable_before_sentence", func(t *testing.T) {
		cases := []struct {
			fail   fakellm.FailMode
			reason llm.Reason
		}{
			{fakellm.FailHTTP402, llm.ReasonInsufficientBalance},
			{fakellm.FailHTTP429, llm.ReasonHTTP429},
			{fakellm.FailHTTP500, llm.ReasonHTTP5xx},
			{fakellm.FailStreamErrorBeforeText, llm.ReasonStreamError},
		}
		for _, c := range cases {
			setup(fakellm.Config{Fail: c.fail}, pipelineOptions{})

			tp.describe(t)

			// An ordinary answer by the template, not an error
			end := requireAnswer(t, templateText(t), protocol.DegradedLLMUnavailable)
			require.True(t, end.Hazard, c.fail)
			require.NotNil(t, find[*protocol.AnswerStart](t, tp.sink).Audio, c.fail)
			record := tp.lastRecord(t)
			require.Equal(t, StatusDegraded, record.Status, c.fail)
			require.Equal(t, string(c.reason), record.LLMError, c.fail)
			// No repeated request, also for 429 with Retry-After
			require.Len(t, fake.Requests(), 1, c.fail)
		}
	})

	t.Run("must_pass_llm_unavailable_after_sentence", func(t *testing.T) {
		cases := []struct {
			fail   fakellm.FailMode
			reason llm.Reason
		}{
			{fakellm.FailStreamErrorAfterSentence, llm.ReasonStreamError},
			{fakellm.FailStream402AfterSentence, llm.ReasonInsufficientBalance},
			{fakellm.FailDisconnectAfterSentence, llm.ReasonDisconnect},
		}
		for _, c := range cases {
			setup(fakellm.Config{Fail: c.fail}, pipelineOptions{})

			tp.describe(t)

			// The answer is closed with the ready sentence
			requireAnswer(t, fakeFirstSentence, protocol.DegradedLLMUnavailable)
			require.Equal(t, []string{fakeFirstSentence}, tp.tts.Texts(), c.fail)
			require.Positive(t, tp.sink.frames(), c.fail)
			record := tp.lastRecord(t)
			require.Equal(t, StatusDegraded, record.Status, c.fail)
			require.Equal(t, string(c.reason), record.LLMError, c.fail)
			require.Len(t, fake.Requests(), 1, c.fail)
		}
	})

	t.Run("must_fail_upstream_timeout", func(t *testing.T) {
		setup(fakellm.Config{FirstTokenDelayMS: 5000}, pipelineOptions{llmTTFTMS: 5000, llmTotalMS: 6000, requestDeadlineMS: 300})

		started := time.Now()
		tp.describe(t)

		require.Less(t, time.Since(started), 2*time.Second)
		requireError(t, protocol.CodeUpstreamTimeout)
		require.Equal(t, "", tp.lastRecord(t).LLMError)
	})
}
