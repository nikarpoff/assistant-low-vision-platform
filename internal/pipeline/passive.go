package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// errPassiveUnavailable is the text of the error of a passive request while its pipeline is not available.
const errPassiveUnavailable = "passive pipeline is not available"

// PassiveRequest is a describe request of the passive mode with its snapshot (PLT-5).
type PassiveRequest struct {
	RequestID uuid.UUID
	SessionID string
	DeviceID  string
	Image     []byte
	// StartedAt is when describe came, ReceivedAt - when the snapshot came: the full input.
	StartedAt  time.Time
	ReceivedAt time.Time
}

// Passive analyzes the snapshot of the passive mode. accepted is sent by the session.
func (p *Pipeline) Passive(ctx context.Context, req *PassiveRequest, sink Sink) {
	r := p.newRun(ctx, KindPassive, req.RequestID, req.SessionID, req.ReceivedAt, sink)
	defer r.cancel()
	r.timings.Set(StageReceive, req.ReceivedAt.Sub(req.StartedAt))
	r.record.ImageBytes = len(req.Image)
	r.fail(protocol.NewError(protocol.CodeInternal, errPassiveUnavailable), errors.New(errPassiveUnavailable))
	p.finish(r)
}
