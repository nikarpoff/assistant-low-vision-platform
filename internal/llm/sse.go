package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// SSE format of the provider stream (PLT-15, LLM-1).
const (
	ssePrefixData    = "data:"
	ssePrefixComment = ":"
	sseDone          = "[DONE]"
	// sseInitialBuffer and sseMaxLine bound one line of the stream: a chunk is far shorter than 1 MiB.
	sseInitialBuffer = 64 << 10
	sseMaxLine       = 1 << 20
)

// jsonNull is the JSON literal that means "no value".
var jsonNull = []byte("null")

// Usage is the token count from the usage chunk (a chunk with empty choices).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chunk is one chat.completion.chunk of the stream. Only the fields the client uses are decoded.
type chunk struct {
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint"`
	Choices           []chunkChoice   `json:"choices"`
	Usage             *Usage          `json:"usage"`
	Error             json.RawMessage `json:"error"`
	TraceID           string          `json:"trace_id"`
}

// chunkChoice is a choice of a chunk.
type chunkChoice struct {
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// chunkDelta is the new text of a choice.
type chunkDelta struct {
	Content *string `json:"content"`
}

// providerError is the error object of Polza.ai, both in the body of a non-2xx
// response and in an error event of the stream. The message is not decoded:
// it never goes to the log or to the application (PLT-15).
type providerError struct {
	Code    json.RawMessage `json:"code"`
	TraceID string          `json:"trace_id"`
}

// errorBody is the body of a non-2xx response: {"error": {...}, "trace_id": "..."}.
type errorBody struct {
	Error   json.RawMessage `json:"error"`
	TraceID string          `json:"trace_id"`
}

// sseReader reads the data lines of an SSE stream.
type sseReader struct {
	scanner *bufio.Scanner
}

// newSSEReader reads the stream line by line.
func newSSEReader(body io.Reader) *sseReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, sseInitialBuffer), sseMaxLine)
	return &sseReader{scanner: scanner}
}

// next returns the payload of the next "data:" line; done is true for "data: [DONE]".
// Comments (lines starting with ":"), empty lines and other SSE fields are skipped.
// At the end of the body it returns io.EOF, on a read failure - the error of the body.
func (r *sseReader) next() (payload []byte, done bool, err error) {
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 || bytes.HasPrefix(line, []byte(ssePrefixComment)) {
			continue
		}
		if !bytes.HasPrefix(line, []byte(ssePrefixData)) {
			continue
		}

		payload = bytes.TrimSpace(line[len(ssePrefixData):])
		if string(payload) == sseDone {
			return nil, true, nil
		}
		return payload, false, nil
	}

	if err := r.scanner.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, io.EOF
}

// parseProviderError returns the code and the trace ID of an "error" field.
// ok is false if the field is absent or null. The trace ID of the error object
// wins over the top-level one.
func parseProviderError(raw json.RawMessage, topTraceID string) (code, traceID string, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, jsonNull) {
		return "", "", false
	}

	traceID = topTraceID
	var object providerError
	if err := json.Unmarshal(raw, &object); err == nil {
		code = codeString(object.Code)
		if object.TraceID != "" {
			traceID = object.TraceID
		}
	}
	return code, traceID, true
}

// parseErrorBody returns the code and the trace ID of the body of a non-2xx response; empty strings if the body is not the provider error format.
func parseErrorBody(body []byte) (code, traceID string) {
	var parsed errorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", ""
	}
	code, traceID, _ = parseProviderError(parsed.Error, parsed.TraceID)
	return code, traceID
}

// codeString returns an error code given as a JSON string ("402") or a number (402) as text.
func codeString(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}
