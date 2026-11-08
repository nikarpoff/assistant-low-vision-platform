package pipeline

import (
	"errors"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// providerFailure returns the failure of the language model provider (PLT-15) behind
// the error of the describer, nil if the error is not one: no error, the end of the
// request or a failure of the gateway itself.
//
// What the pipeline does with it is PLT-9: before the first ready sentence the answer is
// built by the template (DSC-4), after it the answer is closed with the ready sentences.
// A ready sentence is one cut from the stream by PLT-7 and given to TTS; the stream that
// broke right after the end of a sentence has completed it. Either way the answer is
// degraded: llm_timeout for a timeout, llm_unavailable for any other failure. The request
// to the provider is never repeated.
func providerFailure(err error) *llm.Error {
	var failure *llm.Error
	if err == nil || !errors.As(err, &failure) {
		return nil
	}
	return failure
}

// degradedReason maps a provider failure to the degraded reason of answer.end (PLT-9).
func degradedReason(failure *llm.Error) protocol.DegradedReason {
	if failure.Reason.IsTimeout() {
		return protocol.DegradedLLMTimeout
	}
	return protocol.DegradedLLMUnavailable
}

// degrade marks the answer degraded.
func (r *run) degrade(reason protocol.DegradedReason) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.degradeLocked(reason)
}

// degradeLocked marks the answer degraded; r.mu must be held. answer.end has one
// degraded_reason: a failure of the language model wins over a failure of TTS, since
// the share of the template fallbacks is the measure of the provider (EXP-8) and the
// missing audio is told by answer.start with audio null anyway.
func (r *run) degradeLocked(reason protocol.DegradedReason) {
	if reason == protocol.DegradedTTSUnavailable && r.degradedReason != protocol.DegradedNone {
		return
	}
	r.degradedReason = reason
}
