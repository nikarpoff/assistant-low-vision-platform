package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// errAskUnavailable is the text of the error of a question while its pipeline is not available.
const errAskUnavailable = "question pipeline is not available"

// AskRequest is a question (PROTO-7). The session starts it at ask.start and feeds it:
// the snapshot as soon as it comes, then the whole audio at ask.end.
type AskRequest struct {
	RequestID uuid.UUID
	SessionID string
	DeviceID  string
	Trigger   protocol.AskTrigger
	WithImage bool
	// StartedAt is when ask.start came.
	StartedAt time.Time
	// Image gives the snapshot when it comes (only WithImage); it is closed without a
	// value if the question ends without a snapshot.
	Image <-chan []byte
	// End gives the end of the question with all its audio.
	End <-chan AskEnd
}

// AskEnd is the end of a question: ask.end and the audio collected in memory.
type AskEnd struct {
	// Audio is the question, PCM s16le mono 16 kHz: the AUDIO_IN frames in order.
	Audio []byte
	// AudioMS is audio_ms of ask.end.
	AudioMS float64
	// ReceivedAt is when ask.end came: the full input.
	ReceivedAt time.Time
}

// Ask answers a question. accepted is sent by the session at ask.end.
func (p *Pipeline) Ask(ctx context.Context, req *AskRequest, sink Sink) {
	var end AskEnd
	select {
	case end = <-req.End:
	case <-ctx.Done():
		status := StatusCancelled
		if errors.Is(context.Cause(ctx), ErrRejected) {
			status = StatusError
		}
		p.deps.Metrics.Requests.WithLabelValues(KindAsk, status).Inc()
		return
	}

	r := p.newRun(ctx, KindAsk, req.RequestID, req.SessionID, end.ReceivedAt, sink)
	defer r.cancel()
	r.fail(protocol.NewError(protocol.CodeInternal, errAskUnavailable), errors.New(errAskUnavailable))
	p.finish(r)
}
