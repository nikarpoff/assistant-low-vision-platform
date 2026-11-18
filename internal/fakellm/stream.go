package fakellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// SSE values of the stream in the Polza.ai format.
const (
	contentTypeEventStream = "text/event-stream"
	headerCacheControl     = "Cache-Control"
	cacheControlNoCache    = "no-cache"
	commentProcessing      = ": PROCESSING"
	dataDone               = "[DONE]"
	chunkObject            = "chat.completion.chunk"
	chunkID                = "chatcmpl-fake"
	errorChunkID           = "error_fake"
	roleAssistant          = "assistant"
	finishReasonStop       = "stop"
	finishReasonError      = "error"
	// charsPerToken is a rough size of a prompt token for the usage chunk.
	charsPerToken = 4
	// sentenceEnds end a sentence when a space or the end of the text follows (PLT-7).
	sentenceEnds = ".!?…"
)

// streamChunk is a chat.completion.chunk.
type streamChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint"`
	Choices           []chunkChoice `json:"choices"`
	Usage             *chunkUsage   `json:"usage,omitempty"`
}

// errorChunk is an error event of the stream, as in the Polza.ai documentation.
type errorChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Error   errorObject   `json:"error"`
	TraceID string        `json:"trace_id"`
	Choices []chunkChoice `json:"choices"`
}

// chunkChoice is the only choice of a chunk.
type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// chunkDelta is the new text of the choice.
type chunkDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

// chunkUsage is the token count of the usage chunk.
type chunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// sseWriter writes the events of one stream and flushes each of them at once.
type sseWriter struct {
	writer  io.Writer
	flusher http.Flusher
	model   string
	created int64
}

// stream writes the answer, or the failure of the mode, as an SSE stream with status 200.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, prompt int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, msgInternalError)
		return
	}

	w.Header().Set(headerContentType, contentTypeEventStream)
	w.Header().Set(headerCacheControl, cacheControlNoCache)
	w.WriteHeader(http.StatusOK)

	sse := &sseWriter{writer: w, flusher: flusher, model: s.model, created: time.Now().Unix()}
	sse.write(commentProcessing)

	switch s.fail {
	case FailEmpty:
		sse.chunk(chunkDelta{}, finishReasonStop)
		sse.data(dataDone)
		return
	case FailStreamErrorBeforeText:
		sse.errorEvent(CodeServiceUnavailable, msgProviderDisconnected)
		sse.data(dataDone)
		return
	}

	// The role chunk comes at once, the text - after the first token delay
	sse.chunk(chunkDelta{Role: roleAssistant, Content: new(string)}, "")
	ctx := r.Context()
	if !wait(ctx, s.firstTokenDelay) {
		return
	}

	text := s.text
	if s.failsAfterSentence() {
		text = firstSentence(text)
	}
	tokens := splitTokens(text)
	for i, token := range tokens {
		if i > 0 && !wait(ctx, s.tokenDelay) {
			return
		}
		sse.chunk(chunkDelta{Content: &token}, "")
	}

	switch s.fail {
	case FailStreamErrorAfterSentence:
		sse.errorEvent(CodeServiceUnavailable, msgProviderDisconnected)
		sse.data(dataDone)
		return
	case FailStream402AfterSentence:
		sse.errorEvent(CodeInsufficientBalance, msgInsufficientBalance)
		sse.data(dataDone)
		return
	case FailDisconnectAfterSentence:
		closeConnection(w, flusher)
		return
	}

	sse.chunk(chunkDelta{}, finishReasonStop)
	sse.usage(chunkUsage{PromptTokens: prompt, CompletionTokens: len(tokens), TotalTokens: prompt + len(tokens)})
	sse.data(dataDone)
}

// failsAfterSentence reports whether the mode breaks the stream after the first sentence.
func (s *Server) failsAfterSentence() bool {
	return s.fail == FailStreamErrorAfterSentence || s.fail == FailStream402AfterSentence || s.fail == FailDisconnectAfterSentence
}

// chunk writes a chunk with one choice; an empty finishReason is written as null.
func (w *sseWriter) chunk(delta chunkDelta, finishReason string) {
	choice := chunkChoice{Delta: delta}
	if finishReason != "" {
		choice.FinishReason = &finishReason
	}
	w.encode(streamChunk{
		ID:                chunkID,
		Object:            chunkObject,
		Created:           w.created,
		Model:             w.model,
		SystemFingerprint: SystemFingerprint,
		Choices:           []chunkChoice{choice},
	})
}

// usage writes the usage chunk with empty choices.
func (w *sseWriter) usage(usage chunkUsage) {
	w.encode(streamChunk{
		ID:                chunkID,
		Object:            chunkObject,
		Created:           w.created,
		Model:             w.model,
		SystemFingerprint: SystemFingerprint,
		Choices:           []chunkChoice{},
		Usage:             &usage,
	})
}

// errorEvent writes an error event with finish_reason "error"; status stays 200.
func (w *sseWriter) errorEvent(code, message string) {
	finishReason := finishReasonError
	w.encode(errorChunk{
		ID:      errorChunkID,
		Object:  chunkObject,
		Error:   errorObject{Code: code, Message: message},
		TraceID: TraceID,
		Choices: []chunkChoice{{Delta: chunkDelta{Content: new(string)}, FinishReason: &finishReason}},
	})
}

// encode writes a "data: <JSON>" event.
func (w *sseWriter) encode(payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		logger.Debug(logWriteFailureFormat, err)
		return
	}
	w.data(string(encoded))
}

// data writes a "data: <payload>" event.
func (w *sseWriter) data(payload string) {
	w.write("data: " + payload)
}

// write writes one line of the stream with the empty line after it and flushes it.
func (w *sseWriter) write(line string) {
	if _, err := io.WriteString(w.writer, line+"\n\n"); err != nil {
		return
	}
	w.flusher.Flush()
}

// closeConnection breaks the connection without the end of the chunked body, as a lost provider would.
// Without a hijackable connection (HTTP/2) the stream just ends without finish_reason and [DONE].
func closeConnection(w http.ResponseWriter, flusher http.Flusher) {
	flusher.Flush()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// wait sleeps for the delay; false if the client went away earlier.
func wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// splitTokens splits the text into words with their leading spaces; the tokens joined give the text back.
func splitTokens(text string) []string {
	var tokens []string
	start := 0
	previousSpace := true
	for i, r := range text {
		space := unicode.IsSpace(r)
		if space && !previousSpace && i > start {
			tokens = append(tokens, text[start:i])
			start = i
		}
		previousSpace = space
	}
	if start < len(text) {
		tokens = append(tokens, text[start:])
	}
	return tokens
}

// firstSentence returns the text up to the end of the first sentence; the whole text if there is no end.
func firstSentence(text string) string {
	for i, r := range text {
		if !strings.ContainsRune(sentenceEnds, r) {
			continue
		}
		end := i + utf8.RuneLen(r)
		if end == len(text) {
			return text
		}
		next, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(next) {
			return text[:end]
		}
	}
	return text
}

// promptTokens estimates the prompt size for the usage chunk.
func promptTokens(messages []json.RawMessage) int {
	chars := 0
	for _, message := range messages {
		chars += utf8.RuneCount(message)
	}
	return max(chars/charsPerToken, 1)
}
