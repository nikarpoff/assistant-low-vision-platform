package readyhandler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

func TestReady(t *testing.T) {
	var (
		deps       Dependencies
		detector   *ServiceMock
		asr        *ServiceMock
		tts        *ServiceMock
		llm        *StateSourceMock
		journal    *StateSourceMock
		vocabulary *VocabularySourceMock
		response   *httptest.ResponseRecorder
	)

	up := func(context.Context) (*upstream.Info, error) { return &upstream.Info{}, nil }
	down := func(context.Context) (*upstream.Info, error) { return nil, errors.New("connection refused") }

	setup := func() {
		logger.InitTestLogger()
		detector = &ServiceMock{InfoFunc: up}
		asr = &ServiceMock{InfoFunc: up}
		tts = &ServiceMock{InfoFunc: up}
		llm = &StateSourceMock{StateFunc: func() string { return StateOK }}
		journal = &StateSourceMock{StateFunc: func() string { return StateOK }}
		vocabulary = &VocabularySourceMock{LoadedFunc: func() bool { return true }}
		deps = Dependencies{Detector: detector, ASR: asr, TTS: tts, LLM: llm, Journal: journal, Vocabulary: vocabulary, DetectorRequired: true}
		response = httptest.NewRecorder()
	}

	serve := func(t *testing.T) Response {
		NewHandler(deps).GetReady(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		body := Response{}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		return body
	}

	t.Run("must_pass_all_ok", func(t *testing.T) {
		setup()

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.JSONEq(t, `{"status":"ready","detector":"ok","asr":"ok","tts":"ok","llm":"ok","journal":"ok","vocabulary":"loaded"}`, response.Body.String())
		require.Equal(t, StatusReady, body.Status)
		require.Len(t, detector.InfoCalls(), 1)
		require.Len(t, asr.InfoCalls(), 1)
		require.Len(t, tts.InfoCalls(), 1)
	})

	t.Run("must_pass_llm_down", func(t *testing.T) {
		setup()
		llm.StateFunc = func() string { return StateDown }

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, StateDown, body.LLM)
		require.Equal(t, StatusReady, body.Status)
	})

	t.Run("must_pass_template_llm_disabled", func(t *testing.T) {
		setup()
		llm.StateFunc = func() string { return StateDisabled }

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, StateDisabled, body.LLM)
	})

	t.Run("must_pass_journal_disabled", func(t *testing.T) {
		setup()
		journal.StateFunc = func() string { return StateDisabled }

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, StateDisabled, body.Journal)
	})

	t.Run("must_pass_journal_down", func(t *testing.T) {
		setup()
		journal.StateFunc = func() string { return StateDown }

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, StateDown, body.Journal)
	})

	t.Run("must_pass_vlm_without_detector", func(t *testing.T) {
		setup()
		deps.DetectorRequired = false
		detector.InfoFunc = down

		body := serve(t)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, StateDown, body.Detector)
		require.Equal(t, StatusReady, body.Status)
	})

	t.Run("must_fail_detector_down", func(t *testing.T) {
		setup()
		detector.InfoFunc = down

		body := serve(t)

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, StateDown, body.Detector)
		require.Equal(t, StatusNotReady, body.Status)
	})

	t.Run("must_fail_asr_down", func(t *testing.T) {
		setup()
		asr.InfoFunc = down

		body := serve(t)

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, StateDown, body.ASR)
		require.Equal(t, StatusNotReady, body.Status)
	})

	t.Run("must_fail_tts_down", func(t *testing.T) {
		setup()
		tts.InfoFunc = down

		body := serve(t)

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, StateDown, body.TTS)
		require.Equal(t, StatusNotReady, body.Status)
	})

	t.Run("must_fail_vocabulary_not_loaded", func(t *testing.T) {
		setup()
		vocabulary.LoadedFunc = func() bool { return false }

		body := serve(t)

		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, VocabularyNotLoaded, body.Vocabulary)
		require.Equal(t, StatusNotReady, body.Status)
	})
}
