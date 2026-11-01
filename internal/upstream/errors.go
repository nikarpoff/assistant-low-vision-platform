package upstream

import "errors"

// Typed errors of the calls to the model services (PLT-15). The pipeline maps
// them to the degradation of PLT-9; every one of them is counted in
// alv_upstream_errors_total{upstream, reason} with the reason named below.
var (
	// ErrBusy is a 503 answer: the queue of the service is full (SVC-8). Reason "busy".
	ErrBusy = errors.New("upstream busy")
	// ErrUnavailable is another 5xx answer, a failed connection or a broken stream. Reason "unavailable".
	ErrUnavailable = errors.New("upstream unavailable")
	// ErrTimeout is a call that did not finish (or did not give the first chunk) within its timeout (PLT-8). Reason "timeout".
	ErrTimeout = errors.New("upstream timeout")
	// ErrUnauthorized is a 401 answer: the token of the service is wrong (SVC-3). Reason "unauthorized".
	ErrUnauthorized = errors.New("upstream unauthorized")
	// ErrRejected is another non-2xx answer: the service did not accept the input (400, 413, 415, 422). Reason "rejected".
	ErrRejected = errors.New("upstream rejected request")
	// ErrInvalidResponse is a 2xx answer that cannot be used: wrong Content-Type, broken JSON, empty stream. Reason "invalid_response".
	ErrInvalidResponse = errors.New("invalid upstream response")
)

// errCallTimeout is the cancellation cause of a call whose own timeout expired.
var errCallTimeout = errors.New("call timeout expired")

// Texts of the errors, shared by the code and the tests.
const (
	errBaseURLFormat          = "invalid base URL of %s: an absolute http or https URL is required: url=%q"
	errTimeoutValueFormat     = "invalid timeout of %s: a positive duration is required: timeout=%s"
	errStatusFormat           = "%w: upstream=%s status=%d detail=%q"
	errTimeoutFormat          = "%w: upstream=%s timeout=%s"
	errTransportFormat        = "%w: upstream=%s: %w"
	errInterruptedFormat      = "upstream call interrupted: upstream=%s: %w"
	errContentTypeFormat      = "%w: upstream=%s content_type=%q expected=%s"
	errResponseTooLargeFormat = "%w: upstream=%s: body exceeds %d bytes"
	errDecodeFormat           = "%w: upstream=%s: %w"
	errEmptyStreamFormat      = "%w: upstream=%s: empty stream"
	errBuildRequestFormat     = "failed to build request to %s: %w"
)
