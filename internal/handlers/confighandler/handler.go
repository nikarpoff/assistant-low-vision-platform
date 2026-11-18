// Package confighandler serves GET /v1/config: the current configuration snapshot of the
// stand (PLT-12, PLT-13). The route goes behind adminauth.Middleware.
package confighandler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
)

// Texts of the answers, shared by the code and the tests.
const (
	msgInternalError = "internal server error"
	msgNotCollected  = "configuration snapshot is not collected yet"
)

//go:generate moq -out config_source_moq_test.go . ConfigSource

// ConfigSource gives the current configuration snapshot (*snapshot.Collector).
type ConfigSource interface {
	Current() *snapshot.Snapshot
}

// ErrorResponse is the body of an error answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Handler serves GET /v1/config.
type Handler struct {
	config ConfigSource
}

// NewHandler creates a new Handler.
func NewHandler(config ConfigSource) *Handler {
	return &Handler{config: config}
}

// GetConfig handles GET /v1/config
//
// The answer is {"config_id": "...", "collected_at": "<UTC>", "config": <snapshot of PLT-12>}; the
// variant of placement is the top-level field placement of config. The snapshot has no secrets.
// Codes: 200 - the snapshot; 503 - not collected yet; 500 - other errors.
func (h *Handler) GetConfig(w http.ResponseWriter, _ *http.Request) {
	current := h.config.Current()
	if current == nil {
		respondWithJSON(w, http.StatusServiceUnavailable, &ErrorResponse{Error: msgNotCollected})
		return
	}

	body, err := json.Marshal(current)
	if err != nil {
		logger.Error(fmt.Errorf("failed to encode configuration snapshot: %w", err))
		respondWithJSON(w, http.StatusInternalServerError, &ErrorResponse{Error: msgInternalError})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
