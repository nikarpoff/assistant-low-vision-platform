package llm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Reason is the kind of a failed language model request (PLT-15). It is written
// to the journal as llm_error and is the reason label of
// alv_upstream_errors_total{upstream="llm"}.
type Reason string

// Reasons of a failed request, one per row of the PLT-15 outcome table.
const (
	ReasonInsufficientBalance Reason = "insufficient_balance"
	ReasonHTTP429             Reason = "http_429"
	ReasonHTTP5xx             Reason = "http_5xx"
	ReasonHTTP4xx             Reason = "http_4xx"
	ReasonStreamError         Reason = "stream_error"
	ReasonDisconnect          Reason = "disconnect"
	ReasonEmpty               Reason = "empty"
	ReasonTimeoutTTFT         Reason = "timeout_ttft"
	ReasonTimeoutTotal        Reason = "timeout_total"
)

// Error categories for the degradation of PLT-9: a timeout gives
// degraded_reason=llm_timeout, any other failure - llm_unavailable.
var (
	// ErrTimeout matches the failures with ReasonTimeoutTTFT and ReasonTimeoutTotal.
	ErrTimeout = errors.New("language model timeout")
	// ErrUnavailable matches every other provider failure of the PLT-15 table.
	ErrUnavailable = errors.New("language model unavailable")
	// ErrCanceled is returned when the context of the caller ends the request
	// (a cancelled request or the request deadline); it is not a provider failure.
	ErrCanceled = errors.New("language model request canceled")
)

// Provider error codes that mean insufficient funds (PLT-15). Polza.ai does not
// name the string code of 402, so all three spellings are accepted.
var insufficientBalanceCodes = []string{"INSUFFICIENT_BALANCE", "402", "PAYMENT_REQUIRED"}

// errorFormat is the text of Error. The provider message is never part of it:
// only the status, the reason, the code and the trace ID go to the log (PLT-15).
const errorFormat = "language model request failed: reason=%s status=%d code=%s trace_id=%s"

// Error is a failed language model request with the details for the log, the journal and the metrics.
type Error struct {
	// Reason is the outcome of the PLT-15 table.
	Reason Reason
	// StatusCode is the HTTP status of the response; 200 for an error inside the stream, 0 without a response.
	StatusCode int
	// Code is the provider error code (error.code), empty if there is none.
	Code string
	// TraceID is the provider trace ID, empty if there is none.
	TraceID string
	// Err is the cause: a transport error or a timeout; nil for an error reported by the provider.
	Err error
}

// Error returns the reason, the status, the code and the trace ID without the provider message.
func (e *Error) Error() string {
	return fmt.Sprintf(errorFormat, e.Reason, e.StatusCode, e.Code, e.TraceID)
}

// Unwrap returns the cause of the failure.
func (e *Error) Unwrap() error {
	return e.Err
}

// Is reports whether the failure belongs to the ErrTimeout or ErrUnavailable category.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrTimeout:
		return e.Reason.IsTimeout()
	case ErrUnavailable:
		return !e.Reason.IsTimeout()
	default:
		return false
	}
}

// IsTimeout reports whether the reason is a timeout of the first token or of the whole generation.
func (r Reason) IsTimeout() bool {
	return r == ReasonTimeoutTTFT || r == ReasonTimeoutTotal
}

// reasonForStatus maps a non-2xx HTTP status received before the stream to a reason.
func reasonForStatus(status int) Reason {
	switch {
	case status == http.StatusPaymentRequired:
		return ReasonInsufficientBalance
	case status == http.StatusTooManyRequests:
		return ReasonHTTP429
	case status >= 500 && status <= 599:
		return ReasonHTTP5xx
	default:
		return ReasonHTTP4xx
	}
}

// reasonForStreamError maps the code of an error event inside the stream to a reason.
func reasonForStreamError(code string) Reason {
	for _, balanceCode := range insufficientBalanceCodes {
		if strings.EqualFold(code, balanceCode) {
			return ReasonInsufficientBalance
		}
	}
	return ReasonStreamError
}
