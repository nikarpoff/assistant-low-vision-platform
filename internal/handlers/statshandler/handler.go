// Package statshandler serves GET /v1/stats: the statistics of the journal (PLT-13).
// The route goes behind adminauth.Middleware.
package statshandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/utils"
)

// Query parameters of GET /v1/stats.
const (
	querySince    = "since"
	queryUntil    = "until"
	queryKind     = "kind"
	queryConfigID = "config_id"
)

const msgInternalError = "internal server error"

// StatsService computes the statistics of /v1/stats.
//
//go:generate moq -out stats_service_moq_test.go . StatsService
type StatsService interface {
	GetStats(ctx context.Context, req *stats.GetStatsRequest) (*stats.GetStatsResponse, error)
}

// Handler serves GET /v1/stats.
type Handler struct {
	service   StatsService
	validator *utils.Validator
}

// NewHandler creates a new Handler.
func NewHandler(service StatsService) *Handler {
	return &Handler{
		service:   service,
		validator: utils.NewValidator().WithJsonFieldNames(),
	}
}

// GetStats handles GET /v1/stats?since=&until=&kind=&config_id=
//
// Codes: 200 - statistics; 422 - a parameter is not valid; 503 - the journal is disabled; 500 - other errors.
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	req := stats.GetStatsRequest{
		Since:    query.Get(querySince),
		Until:    query.Get(queryUntil),
		Kind:     query.Get(queryKind),
		ConfigID: query.Get(queryConfigID),
	}

	validationResult := h.validator.Validate(req)
	if validationResult.Error != nil {
		respondWithJSON(w, http.StatusUnprocessableEntity, &stats.ErrorResponse{Error: validationResult.Message})
		return
	}

	resp, err := h.service.GetStats(r.Context(), &req)
	if err != nil {
		switch {
		case errors.Is(err, stats.ErrValidationFailed):
			respondWithJSON(w, http.StatusUnprocessableEntity, &stats.ErrorResponse{Error: err.Error()})
		case errors.Is(err, stats.ErrJournalDisabled):
			respondWithJSON(w, http.StatusServiceUnavailable, &stats.ErrorResponse{Error: stats.ErrJournalDisabled.Error()})
		default:
			logger.Error(fmt.Errorf("failed to get stats: %w", err))
			respondWithJSON(w, http.StatusInternalServerError, &stats.ErrorResponse{Error: msgInternalError})
		}
		return
	}

	respondWithJSON(w, http.StatusOK, resp)
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
