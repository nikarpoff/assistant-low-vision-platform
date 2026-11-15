// Package readyhandler serves GET /ready: the state of every dependency of the gateway (PLT-13).
package readyhandler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

// States of a dependency.
const (
	StateOK       = "ok"
	StateDown     = "down"
	StateDisabled = "disabled"
)

// States of the vocabulary of the detector.
const (
	VocabularyLoaded    = "loaded"
	VocabularyNotLoaded = "not_loaded"
)

// Status of the whole gateway.
const (
	StatusReady    = "ready"
	StatusNotReady = "not_ready"
)

//go:generate moq -out service_moq_test.go . Service

// Service is a model service checked by GET /v1/info (*detector.Client, *asr.Client, *tts.Client).
type Service interface {
	Info(ctx context.Context) (*upstream.Info, error)
}

//go:generate moq -out state_source_moq_test.go . StateSource

// StateSource gives the last known state of a dependency that degrades instead of failing the
// requests: the provider of the language model and the journal ("ok", "down" or "disabled").
type StateSource interface {
	State() string
}

//go:generate moq -out vocabulary_source_moq_test.go . VocabularySource

// VocabularySource tells whether the vocabulary of the detector is loaded (*vocabulary.Loader).
type VocabularySource interface {
	Loaded() bool
}

// Dependencies are what /ready checks.
type Dependencies struct {
	Detector   Service
	ASR        Service
	TTS        Service
	LLM        StateSource
	Journal    StateSource
	Vocabulary VocabularySource
	// DetectorRequired is false in the vlm mode: the scene is described from the snapshot alone (PLT-9).
	DetectorRequired bool
}

// Response is the body of GET /ready.
type Response struct {
	Status     string `json:"status"`
	Detector   string `json:"detector"`
	ASR        string `json:"asr"`
	TTS        string `json:"tts"`
	LLM        string `json:"llm"`
	Journal    string `json:"journal"`
	Vocabulary string `json:"vocabulary"`
}

// Handler serves GET /ready.
type Handler struct {
	deps Dependencies
}

// NewHandler creates a new Handler.
func NewHandler(deps Dependencies) *Handler {
	return &Handler{deps: deps}
}

// GetReady handles GET /ready
//
// The model services are asked at once (GET /v1/info). Codes: 200 - the vocabulary is loaded and ASR,
// TTS and the detector (except in the vlm mode) answer; 503 - otherwise. The states of llm and journal
// are shown but do not change the code: the gateway degrades without them (PLT-9).
func (h *Handler) GetReady(w http.ResponseWriter, r *http.Request) {
	response := Response{
		LLM:        h.deps.LLM.State(),
		Journal:    h.deps.Journal.State(),
		Vocabulary: VocabularyNotLoaded,
	}
	if h.deps.Vocabulary.Loaded() {
		response.Vocabulary = VocabularyLoaded
	}

	var wg sync.WaitGroup
	for _, target := range []struct {
		service Service
		state   *string
	}{
		{h.deps.Detector, &response.Detector},
		{h.deps.ASR, &response.ASR},
		{h.deps.TTS, &response.TTS},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*target.state = check(r.Context(), target.service)
		}()
	}
	wg.Wait()

	ready := response.Vocabulary == VocabularyLoaded &&
		response.ASR == StateOK &&
		response.TTS == StateOK &&
		(response.Detector == StateOK || !h.deps.DetectorRequired)

	code := http.StatusOK
	response.Status = StatusReady
	if !ready {
		code = http.StatusServiceUnavailable
		response.Status = StatusNotReady
		logger.Debug("gateway is not ready: detector=%s asr=%s tts=%s vocabulary=%s",
			response.Detector, response.ASR, response.TTS, response.Vocabulary)
	}
	respondWithJSON(w, code, &response)
}

// check asks the service for /v1/info.
func check(ctx context.Context, service Service) string {
	if _, err := service.Info(ctx); err != nil {
		return StateDown
	}
	return StateOK
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
