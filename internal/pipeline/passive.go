package pipeline

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// warningEnd ends the text of a warning in answer.text; the phrase goes to TTS without it.
const warningEnd = "."

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

// Passive analyzes the snapshot of the passive mode (PLT-5): detector, hazard rules and
// the cooldown of the device, then a warning by the phrases of the rules - answer.start
// with kind warning, priority high for a danger - or passive.clear with the reason. The
// language model is never called. accepted is sent by the session.
//
// The request is skipped while a user request or another passive request of the session
// is in progress, and its result is stale once APP_PASSIVE_DEADLINE_MS has passed since
// the snapshot came, by the clock of the gateway (PROTO-8).
func (p *Pipeline) Passive(ctx context.Context, req *PassiveRequest, sink Sink) {
	r := p.newRun(ctx, KindPassive, req.RequestID, req.SessionID, req.ReceivedAt, sink)
	defer r.cancel()
	r.timings.Set(StageReceive, req.ReceivedAt.Sub(req.StartedAt))
	r.record.ImageBytes = len(req.Image)

	release, ok := p.beginPassive(req.SessionID)
	if !ok {
		p.clear(r, protocol.ClearSkipped)
		p.finish(r)
		return
	}
	defer release()
	r.staleAt = req.ReceivedAt.Add(p.cfg.PassiveDeadline)
	if !time.Now().Before(r.staleAt) {
		p.clear(r, protocol.ClearStale)
		p.finish(r)
		return
	}

	detectCtx, cancelDetect := context.WithDeadline(r.ctx, r.staleAt)
	detectStarted := time.Now()
	detected, err := p.deps.Detector.Detect(detectCtx, r.id, req.Image)
	r.timings.Set(StageDetect, time.Since(detectStarted))
	cancelDetect()
	switch {
	case r.cancelled() || r.rejected():
		p.finish(r)
		return
	case !time.Now().Before(r.staleAt):
		p.clear(r, protocol.ClearStale)
		p.finish(r)
		return
	case err != nil:
		r.fail(protocol.NewError(protocol.CodeDetectorUnavailable, errDetectorUnavailable), err)
		p.finish(r)
		return
	}

	warnings, reason := p.warnings(req.DeviceID, detected.Objects)
	if len(warnings) == 0 {
		p.clear(r, reason)
		p.finish(r)
		return
	}
	p.warn(r, warnings, detected.Objects)
	p.finish(r)
}

// warnings returns the hazards to speak now: the level danger, and notice with
// APP_PASSIVE_NOTICES, that the cooldown of the device lets through (PLT-5, PLT-6),
// at most MaxSentences. Without them it returns the reason of passive.clear.
func (p *Pipeline) warnings(deviceID string, objects []detector.Object) ([]hazard.Match, protocol.ClearReason) {
	candidates := []hazard.Match{}
	for _, match := range p.deps.Policy.Evaluate(objects) {
		if match.Level == hazard.LevelDanger || (match.Level == hazard.LevelNotice && p.cfg.PassiveNotices) {
			candidates = append(candidates, match)
		}
	}
	// The cooldown sees every frame, also without hazards: it remembers the distances for far → near
	speak := p.cooldown(deviceID).Apply(candidates, objects)
	switch {
	case len(candidates) == 0:
		return nil, protocol.ClearNoHazard
	case len(speak) == 0:
		return nil, protocol.ClearCooldown
	}
	if len(speak) > MaxSentences {
		speak = speak[:MaxSentences]
	}
	return speak, ""
}

// warn speaks the warnings: answer.start with kind warning, then answer.text and the audio
// of every phrase, then answer.end. A result that got stale before answer.start ends with
// passive.clear stale instead.
func (p *Pipeline) warn(r *run, warnings []hazard.Match, objects []detector.Object) {
	hasHazard := false
	for _, warning := range warnings {
		if warning.Level == hazard.LevelDanger {
			hasHazard = true
		}
	}
	if hasHazard {
		r.priority = protocol.PriorityHigh
	}

	speech := newSpeech(r.ctx, p.deps.Synthesizer, r.id, r.timings)
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		p.emit(r, speech, protocol.AnswerKindWarning)
	}()
	for _, warning := range warnings {
		logger.Debug("warning ready: request_id=%s rule=%s phrase=%s", r.id, warning.Rule, warning.Phrase)
		speech.SayAs(warning.Phrase, warning.Phrase+warningEnd)
	}
	speech.Close()
	<-emitted

	switch {
	case r.cancelled() || r.rejected():
		return
	case r.isStale():
		p.clear(r, protocol.ClearStale)
		return
	case !r.answerStarted():
		r.fail(protocol.NewError(protocol.CodeUpstreamTimeout, errDeadlineExceeded), context.Cause(r.ctx))
		return
	}
	p.end(r, p.sceneObjects(objects, protocol.FocusAll), hasHazard)
}

// clear ends the passive request without a warning.
func (p *Pipeline) clear(r *run, reason protocol.ClearReason) {
	switch reason {
	case protocol.ClearSkipped:
		r.status = StatusSkipped
	case protocol.ClearStale:
		r.status = StatusStale
	default:
		r.status = StatusClear
	}
	if r.cancelled() || r.rejected() {
		return
	}
	r.sink.Send(&protocol.PassiveClear{RequestID: r.uuid, Reason: reason})
	logger.Info("passive request cleared: request_id=%s reason=%s", r.id, reason)
}

// deviceCooldown is the hazard cooldown of a device and its last use.
type deviceCooldown struct {
	cooldown *hazard.Cooldown
	used     time.Time
}

// cooldown returns the hazard cooldown of the device. It is kept by device_id, like the
// history, so a reconnect does not repeat the warnings. A cooldown unused for a whole
// period holds nothing that suppresses a warning any more and is forgotten.
func (p *Pipeline) cooldown(deviceID string) *hazard.Cooldown {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	for id, entry := range p.cooldowns {
		if now.Sub(entry.used) >= p.cfg.HazardCooldown {
			delete(p.cooldowns, id)
		}
	}
	entry, ok := p.cooldowns[deviceID]
	if !ok {
		entry = &deviceCooldown{cooldown: hazard.NewCooldown(p.cfg.HazardCooldown, func() time.Time { return p.now() })}
		p.cooldowns[deviceID] = entry
	}
	entry.used = now
	return entry.cooldown
}

// inProgress counts the requests of a session in progress in the pipelines.
type inProgress struct {
	user    int
	passive int
}

// beginUser marks a user request of the session in progress and returns its end.
func (p *Pipeline) beginUser(sessionID string) func() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.session(sessionID).user++
	return func() { p.leave(sessionID, func(s *inProgress) { s.user-- }) }
}

// beginPassive marks a passive request of the session in progress unless a user or a
// passive request of the session is in progress (PROTO-8); it returns the end of the request.
func (p *Pipeline) beginPassive(sessionID string) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.session(sessionID)
	if s.user > 0 || s.passive > 0 {
		return nil, false
	}
	s.passive++
	return func() { p.leave(sessionID, func(s *inProgress) { s.passive-- }) }, true
}

// leave ends a request of the session; a session without requests in progress is forgotten.
func (p *Pipeline) leave(sessionID string, done func(s *inProgress)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.session(sessionID)
	done(s)
	if s.user <= 0 && s.passive <= 0 {
		delete(p.sessions, sessionID)
	}
}

// session returns the requests in progress of the session, creating them; p.mu must be held.
func (p *Pipeline) session(sessionID string) *inProgress {
	s, ok := p.sessions[sessionID]
	if !ok {
		s = &inProgress{}
		p.sessions[sessionID] = s
	}
	return s
}
