package stats

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrValidationFailed is returned for a query of /v1/stats that cannot be a filter.
var ErrValidationFailed = errors.New("validation failed")

// ErrJournalDisabled is returned by the journal repository while the journal is disabled (empty APP_POSTGRES_DSN).
var ErrJournalDisabled = errors.New("journal is disabled")

// errUntilNotAfterSince is the detail of ErrValidationFailed for an empty time range.
var errUntilNotAfterSince = errors.New("until must be later than since")

// errTimeFormat is the detail of ErrValidationFailed for a time that is not RFC 3339; the argument is the parameter.
const errTimeFormat = "%w: %s must be an RFC 3339 time"

// Repository selects the journal data of the requests matching the filter.
//
//go:generate moq -out repository_moq_test.go . Repository
type Repository interface {
	SelectSample(ctx context.Context, filter *Filter) (*Sample, error)
}

// Service computes the statistics of /v1/stats (PLT-13) from the journal.
type Service struct {
	repository Repository
}

// NewService creates a new Service over the journal repository.
func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// GetStats selects the journal requests by the filters of the query and computes:
//   - n, p50, p95 and mean of every stage and of E2E by request kind;
//   - the number and share of answers (answer.end) by degraded_reason and the number by llm_error;
//   - llm_fallback_share of the scene description answers;
//   - the number of requests by llm_model.
func (s *Service) GetStats(ctx context.Context, req *GetStatsRequest) (*GetStatsResponse, error) {
	filter, err := parseFilter(req)
	if err != nil {
		return nil, err
	}

	sample, err := s.repository.SelectSample(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to select journal sample: %w", err)
	}

	response := newResponse(filter)
	aggregate(response, sample)
	return response, nil
}

// parseFilter converts the query to a filter with UTC times.
func parseFilter(req *GetStatsRequest) (*Filter, error) {
	since, err := parseTime("since", req.Since)
	if err != nil {
		return nil, err
	}

	until, err := parseTime("until", req.Until)
	if err != nil {
		return nil, err
	}

	if since != nil && until != nil && !until.After(*since) {
		return nil, fmt.Errorf("%w: %w", ErrValidationFailed, errUntilNotAfterSince)
	}

	return &Filter{Since: since, Until: until, Kind: req.Kind, ConfigID: req.ConfigID}, nil
}

// parseTime parses an RFC 3339 time to UTC; an empty value gives nil.
func parseTime(name, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf(errTimeFormat, ErrValidationFailed, name)
	}

	parsed = parsed.UTC()
	return &parsed, nil
}

// newResponse creates an empty response that repeats the filter: unused filters are null.
func newResponse(filter *Filter) *GetStatsResponse {
	response := &GetStatsResponse{
		ConfigID:  optionalString(filter.ConfigID),
		Since:     optionalTime(filter.Since),
		Until:     optionalTime(filter.Until),
		Kind:      optionalString(filter.Kind),
		Stages:    map[string]Summary{},
		E2E:       map[string]Summary{},
		Degraded:  map[string]Share{},
		LLMErrors: map[string]int{},
		LLMModels: map[string]int{},
	}

	for _, reason := range degradedReasons {
		response.Degraded[reason] = Share{}
	}

	return response
}

// aggregate fills the response with the statistics of the sample.
func aggregate(response *GetStatsResponse, sample *Sample) {
	for name, values := range groupMeasurements(sample.Stages) {
		response.Stages[name] = Summarize(values)
	}

	for kind, values := range groupMeasurements(sample.E2E) {
		response.E2E[kind] = Summarize(values)
	}

	answers := make([]Answer, 0, len(sample.Requests))
	for _, request := range sample.Requests {
		if request.LLMModel != "" {
			response.LLMModels[request.LLMModel]++
		}

		// only the requests that sent answer.end are answers
		if request.Status != StatusOK && request.Status != StatusDegraded {
			continue
		}

		answers = append(answers, Answer{
			Kind:           request.Kind,
			Intent:         request.Intent,
			DescriberMode:  request.DescriberMode,
			DegradedReason: request.DegradedReason,
		})

		if request.LLMError != "" {
			response.LLMErrors[request.LLMError]++
		}

		if share, ok := response.Degraded[request.DegradedReason]; ok {
			share.N++
			response.Degraded[request.DegradedReason] = share
		}
	}

	if len(answers) > 0 {
		for reason, share := range response.Degraded {
			share.Share = float64(share.N) / float64(len(answers))
			response.Degraded[reason] = share
		}
	}

	response.LLMFallbackShare = FallbackShare(answers)
}

// groupMeasurements groups the values by name.
func groupMeasurements(measurements []Measurement) map[string][]float64 {
	groups := map[string][]float64{}
	for _, measurement := range measurements {
		groups[measurement.Name] = append(groups[measurement.Name], measurement.Value)
	}
	return groups
}

// optionalString returns nil for an empty value.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalTime formats the time as RFC 3339 in UTC; nil stays nil.
func optionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}

	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
