package protocol

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrorCode is the code of the error message (PROTO-11). The application picks the phrase for the user by it.
type ErrorCode string

// Error codes (PROTO-11).
const (
	CodeBadRequest          ErrorCode = "bad_request"
	CodeUnsupportedProtocol ErrorCode = "unsupported_protocol"
	CodePayloadTooLarge     ErrorCode = "payload_too_large"
	CodeBusy                ErrorCode = "busy"
	CodeASREmpty            ErrorCode = "asr_empty"
	CodeASRUnavailable      ErrorCode = "asr_unavailable"
	CodeDetectorUnavailable ErrorCode = "detector_unavailable"
	CodeUpstreamTimeout     ErrorCode = "upstream_timeout"
	CodeInternal            ErrorCode = "internal"
)

// retryableCodes is the retryable column of the PROTO-11 table.
var retryableCodes = map[ErrorCode]bool{
	CodeBadRequest:          false,
	CodeUnsupportedProtocol: false,
	CodePayloadTooLarge:     false,
	CodeBusy:                true,
	CodeASREmpty:            true,
	CodeASRUnavailable:      true,
	CodeDetectorUnavailable: true,
	CodeUpstreamTimeout:     true,
	CodeInternal:            true,
}

// Texts of the protocol errors, shared by the code and the tests.
const (
	errErrorFormat               = "%s: %s"
	errFrameShortFormat          = "frame is shorter than the %d-byte header: %d bytes"
	errFrameVersionFormat        = "unknown frame version 0x%02x"
	errFrameKindFormat           = "unknown frame kind 0x%02x"
	errImageTooLargeFormat       = "image exceeds %d bytes"
	errAudioChunkTooLargeFormat  = "audio chunk exceeds %d bytes"
	errMessageNotObjectFormat    = "message is not a JSON object: %v"
	errMissingType               = "message type is missing"
	errUnknownTypeFormat         = "unknown message type %q"
	errInvalidMessageFormat      = "invalid %s message: %v"
	errUnsupportedProtocolFormat = "unsupported protocol version %d"
	errEncodeMessageFormat       = "failed to encode %s message: %w"
	errNotJSONObject             = "not a JSON object"
	errMillisNotFiniteFormat     = "duration is not a finite number: %v"
	msgInternalError             = "internal server error"
)

// Error is a rejected frame or message or a failed request with its PROTO-11 code.
// Message is the technical text in English that goes to the application.
type Error struct {
	Code    ErrorCode
	Message string
}

// NewError creates an Error with the message built from format and args.
func NewError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error returns the code and the message: "bad_request: message type is missing".
func (e *Error) Error() string {
	return fmt.Sprintf(errErrorFormat, e.Code, e.Message)
}

// Retryable reports whether the application may repeat the request (PROTO-11).
// An unknown code is not retryable.
func (c ErrorCode) Retryable() bool {
	return retryableCodes[c]
}

// CodeOf returns the code of the first *Error in the chain of err, or internal for any other error.
func CodeOf(err error) ErrorCode {
	var protocolErr *Error
	if errors.As(err, &protocolErr) {
		return protocolErr.Code
	}
	return CodeInternal
}

// NewErrorMessage builds the error message for err (PROTO-5). An *Error in the chain gives its
// code and text; any other error gives internal with an impersonal text, its details stay in the log.
// requestID is nil when the error does not belong to a request.
func NewErrorMessage(requestID *uuid.UUID, err error) *ErrorMessage {
	var protocolErr *Error
	if !errors.As(err, &protocolErr) {
		protocolErr = &Error{Code: CodeInternal, Message: msgInternalError}
	}

	return &ErrorMessage{
		RequestID: requestID,
		Code:      protocolErr.Code,
		Message:   protocolErr.Message,
		Retryable: protocolErr.Code.Retryable(),
	}
}
