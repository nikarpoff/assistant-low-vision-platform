package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/intent"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Texts of the question errors that go to the application, shared by the code and the tests.
const (
	errASREmpty       = "no speech recognized"
	errASRUnavailable = "speech recognizer unavailable"
	errIntentFailed   = "intent recognition failed"
)

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

// detection is the result of the detector on the snapshot of a question.
type detection struct {
	// image is false when the question came without a snapshot.
	image      bool
	snapshot   []byte
	receivedAt time.Time
	response   *detector.DetectResponse
	err        error
	duration   time.Duration
}

// Ask answers a question (PLT-3): the detector runs on the snapshot as soon as it comes,
// in parallel with the speech of the user; at ask.end the audio goes to ASR in one body,
// then the intent (PLT-4). The commands the application executes get transcript and an
// empty answer.end; describe, describe_ahead and question get the description of the
// scene with the question and the history. A question without a snapshot uses the last
// scene of the device younger than APP_SCENE_FRESHNESS_MS, otherwise the history alone.
// accepted is sent by the session at ask.end.
func (p *Pipeline) Ask(ctx context.Context, req *AskRequest, sink Sink) {
	defer p.beginUser(req.SessionID)()

	detected := make(chan detection, 1)
	go p.detectQuestion(ctx, req, detected)

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

	transcript, ok := p.recognize(r, end.Audio)
	if !ok {
		p.finish(r)
		return
	}
	r.sink.Send(&protocol.Transcript{RequestID: r.uuid, Text: transcript, Intent: r.intent})

	switch r.intent {
	case protocol.IntentRepeat, protocol.IntentStop, protocol.IntentPassiveOn, protocol.IntentPassiveOff:
		p.command(r)
		p.finish(r)
		return
	}

	scene, ok := p.questionScene(r, req, detected)
	if !ok {
		p.finish(r)
		return
	}
	in := &describer.Input{
		RequestID: r.id,
		Objects:   scene.objects,
		Focus:     protocol.FocusAll,
		History:   p.deps.History.Get(req.DeviceID),
		Image:     scene.snapshot,
	}
	kind := protocol.AnswerKindDescription
	switch r.intent {
	case protocol.IntentDescribeAhead:
		in.Focus = protocol.FocusAhead
	case protocol.IntentQuestion:
		in.Question = transcript
		kind = protocol.AnswerKindAnswer
	}

	text, answered := p.respond(r, in, kind)
	if answered {
		if scene.fresh {
			p.deps.History.AddScene(req.DeviceID, history.Scene{Objects: scene.objects, Description: text, At: scene.at})
		}
		p.deps.History.AddTurn(req.DeviceID,
			history.Turn{Role: history.RoleQuestion, Text: transcript},
			history.Turn{Role: history.RoleAnswer, Text: text},
		)
	}
	p.finish(r)
}

// detectQuestion waits for the snapshot of the question and runs the detector on it at
// once, while the user is still speaking (PLT-3).
func (p *Pipeline) detectQuestion(ctx context.Context, req *AskRequest, detected chan<- detection) {
	var image []byte
	ok := false
	select {
	case image, ok = <-req.Image:
	case <-ctx.Done():
		return
	}
	if !ok {
		detected <- detection{}
		return
	}

	result := detection{image: true, snapshot: image, receivedAt: time.Now()}
	result.response, result.err = p.deps.Detector.Detect(ctx, req.RequestID.String(), image)
	result.duration = time.Since(result.receivedAt)
	detected <- result
}

// recognize transcribes the question and recognizes its intent. An empty transcript is
// the error asr_empty, a failed ASR - asr_unavailable (PROTO-11, PLT-9).
func (p *Pipeline) recognize(r *run, audio []byte) (string, bool) {
	started := time.Now()
	response, err := p.deps.Recognizer.Recognize(r.ctx, r.id, audio)
	r.timings.Set(StageASR, time.Since(started))
	if err != nil {
		r.fail(protocol.NewError(protocol.CodeASRUnavailable, errASRUnavailable), err)
		return "", false
	}
	r.record.Transcript = response.Text

	recognized, err := p.deps.Intents.Recognize(response.Text)
	if errors.Is(err, intent.ErrEmptyText) {
		r.fail(protocol.NewError(protocol.CodeASREmpty, errASREmpty), err)
		return "", false
	}
	if err != nil {
		r.fail(protocol.NewError(protocol.CodeInternal, errIntentFailed), err)
		return "", false
	}
	r.intent = recognized
	logger.Debug("question recognized: request_id=%s intent=%s text=%s", r.id, recognized, response.Text)
	return response.Text, true
}

// command ends a command the application executes: answer.end with an empty text and
// without audio right after transcript (PROTO-7).
func (p *Pipeline) command(r *run) {
	r.status = StatusOK
	r.timings.Set(StageServerTotal, time.Since(r.received))
	r.record.Objects = []byte("[]")
	r.sink.Send(&protocol.AnswerEnd{
		RequestID: r.uuid,
		Intent:    r.intent,
		Objects:   []protocol.SceneObject{},
		Timings:   r.timings.Protocol(),
	})
	logger.Info("request answered: request_id=%s kind=%s status=%s intent=%s", r.id, r.kind, r.status, r.intent)
}

// questionScene is the scene a question is answered by.
type questionScene struct {
	objects  []detector.Object
	snapshot []byte
	// fresh is true for the scene of the snapshot of the question: it goes to the history.
	fresh bool
	at    time.Time
}

// questionScene waits for the detector if the question came with a snapshot; without one
// it takes the last scene of the device younger than APP_SCENE_FRESHNESS_MS, otherwise no
// scene: the answer is built by the history alone (PLT-3).
func (p *Pipeline) questionScene(r *run, req *AskRequest, detected <-chan detection) (questionScene, bool) {
	var result detection
	select {
	case result = <-detected:
	case <-r.ctx.Done():
		r.fail(protocol.NewError(protocol.CodeUpstreamTimeout, errDeadlineExceeded), context.Cause(r.ctx))
		return questionScene{}, false
	}

	if result.image {
		r.record.ImageBytes = len(result.snapshot)
		r.timings.Set(StageDetect, result.duration)
		objects, ok := p.detection(r, result.response, result.err)
		if !ok {
			return questionScene{}, false
		}
		return questionScene{objects: objects, snapshot: result.snapshot, fresh: true, at: result.receivedAt}, true
	}

	if last, ok := p.deps.History.Get(req.DeviceID).LastScene(); ok && time.Since(last.At) <= p.cfg.SceneFreshness {
		logger.Debug("question without a snapshot uses the last scene: request_id=%s age_ms=%d", r.id, time.Since(last.At).Milliseconds())
		return questionScene{objects: last.Objects}, true
	}
	return questionScene{}, true
}
