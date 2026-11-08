// Package pipeline runs the requests of a session (PLT-2, PLT-7, PLT-14): the snapshot
// goes to the detector, the scene is described, the description is cut into sentences
// and every sentence is synthesized and sent to the application as soon as it is ready.
//
// A pipeline sends the messages and the frames of a request to a Sink; the session
// drops them once the request is cancelled (PROTO-8).
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/journal"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Kinds of requests: the kind label of the metrics and the kind of the journal.
const (
	KindDescribe = "describe"
	KindAsk      = "ask"
	KindPassive  = "passive"
)

// Statuses of a finished request (PLT-11): the status label of alv_requests_total.
const (
	StatusOK        = "ok"
	StatusDegraded  = "degraded"
	StatusError     = "error"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
	StatusStale     = "stale"
	StatusClear     = "clear"
)

// Texts of the request errors that go to the application, shared by the code and the tests.
const (
	errDetectorUnavailable = "object detector unavailable"
	errDeadlineExceeded    = "request deadline exceeded"
	errDescribeFailed      = "scene description failed"
	errNoSentences         = "scene description has no sentences"
)

// ErrCancelled is the cancellation cause of a request cancelled by the application:
// by cancel or by a new user request (PROTO-8). The pipeline sends nothing after it
// and records the status cancelled.
var ErrCancelled = errors.New("request cancelled")

// ErrRejected is the cancellation cause of a request the session ended with an error
// of its own, for example a question over max_question_ms. The pipeline sends nothing
// after it and records the status error.
var ErrRejected = errors.New("request rejected")

//go:generate moq -out sink_moq_test.go . Sink

// Sink takes the messages and the AUDIO_OUT frames of one request in their order.
// It drops them once the request is cancelled.
type Sink interface {
	Send(message protocol.Message)
	SendAudio(seq uint32, payload []byte)
}

//go:generate moq -out object_detector_moq_test.go . ObjectDetector

// ObjectDetector finds the objects on a snapshot (*detector.Client).
type ObjectDetector interface {
	Detect(ctx context.Context, requestID string, image []byte) (*detector.DetectResponse, error)
}

//go:generate moq -out scene_describer_moq_test.go . SceneDescriber

// SceneDescriber describes the scene (*describer.Describer).
type SceneDescriber interface {
	Mode() env.DescriberMode
	Describe(ctx context.Context, in *describer.Input, onText llm.TextHandler) (*describer.Result, error)
	Template(in *describer.Input, onText llm.TextHandler) (*describer.Result, error)
}

//go:generate moq -out history_reader_moq_test.go . HistoryReader

// HistoryReader gives the history of a device (*history.Store).
type HistoryReader interface {
	Get(deviceID string) history.History
}

//go:generate moq -out recorder_moq_test.go . Recorder

// Recorder takes the records of the journal without waiting for the database (*journal.Writer).
type Recorder interface {
	Write(record journal.Record)
}

//go:generate moq -out config_source_moq_test.go . ConfigSource

// ConfigSource gives the current configuration snapshot for config_id (*snapshot.Collector).
type ConfigSource interface {
	Current() *snapshot.Snapshot
}

// Config holds the settings of the pipelines.
type Config struct {
	// MaxAnswerChars is APP_MAX_ANSWER_CHARS.
	MaxAnswerChars int
	// RequestDeadline is APP_REQUEST_DEADLINE_MS: the whole user request from the full input.
	RequestDeadline time.Duration
	// AudioOut is the audio format of answer.start when TTS does not name its sample rate.
	AudioOut protocol.AudioFormat
}

// NewConfig takes the settings of the pipelines from the gateway configuration.
func NewConfig(vars *env.Vars) Config {
	return Config{
		MaxAnswerChars:  vars.AppMaxAnswerChars,
		RequestDeadline: time.Duration(vars.AppRequestDeadlineMS) * time.Millisecond,
		AudioOut: protocol.AudioFormat{
			Format:     protocol.AudioFormatPCMS16LE,
			SampleRate: vars.AppAudioOutSampleRate,
			Channels:   1,
		},
	}
}

// Dependencies are the services and the state the pipelines use.
type Dependencies struct {
	Detector    ObjectDetector
	Synthesizer Synthesizer
	Describer   SceneDescriber
	History     HistoryReader
	Policy      *hazard.Policy
	Recorder    Recorder
	Config      ConfigSource
	Metrics     *metrics.Metrics
}

// Pipeline runs the requests of all sessions. It has no state of its own.
type Pipeline struct {
	cfg  Config
	deps Dependencies
}

// NewPipeline creates the pipelines.
func NewPipeline(cfg Config, deps Dependencies) *Pipeline {
	return &Pipeline{cfg: cfg, deps: deps}
}

// DescribeRequest is a describe request with its snapshot (PROTO-6).
type DescribeRequest struct {
	RequestID uuid.UUID
	SessionID string
	DeviceID  string
	Focus     protocol.Focus
	Image     []byte
	// StartedAt is when describe came, ReceivedAt - when the snapshot came: the full input.
	StartedAt  time.Time
	ReceivedAt time.Time
}

// Describe runs the on-demand description (PLT-2): detector, description, sentences,
// TTS and AUDIO_OUT, then answer.end with the timings. accepted is sent by the session.
// It returns when the request is over; a cancelled request sends nothing more.
func (p *Pipeline) Describe(ctx context.Context, req *DescribeRequest, sink Sink) {
	r := p.newRun(ctx, KindDescribe, req.RequestID, req.SessionID, req.ReceivedAt, sink)
	defer r.cancel()
	r.timings.Set(StageReceive, req.ReceivedAt.Sub(req.StartedAt))
	r.record.ImageBytes = len(req.Image)
	r.intent = protocol.IntentDescribe
	if req.Focus == protocol.FocusAhead {
		r.intent = protocol.IntentDescribeAhead
	}

	detectStarted := time.Now()
	detected, err := p.deps.Detector.Detect(r.ctx, r.id, req.Image)
	r.timings.Set(StageDetect, time.Since(detectStarted))
	if err != nil {
		r.fail(protocol.NewError(protocol.CodeDetectorUnavailable, errDetectorUnavailable), err)
		p.finish(r)
		return
	}

	in := &describer.Input{
		RequestID: r.id,
		Objects:   detected.Objects,
		Focus:     req.Focus,
		History:   p.deps.History.Get(req.DeviceID),
		Image:     req.Image,
	}
	p.respond(r, in, protocol.AnswerKindDescription)
	p.finish(r)
}

// run is the state of one request in a pipeline.
type run struct {
	ctx      context.Context
	cancel   context.CancelFunc
	kind     string
	id       string
	uuid     uuid.UUID
	sink     Sink
	received time.Time
	timings  *Timings
	record   *journal.Request
	intent   protocol.Intent

	mu             sync.Mutex
	started        bool // answer.start is sent
	audio          bool // the answer goes with audio
	frames         uint32
	sentences      []string
	degradedReason protocol.DegradedReason
	status         string
	errorCode      protocol.ErrorCode
}

// newRun starts a request: the deadline counts from the full input (PLT-8).
func (p *Pipeline) newRun(ctx context.Context, kind string, id uuid.UUID, sessionID string, received time.Time, sink Sink) *run {
	ctx, cancel := context.WithDeadline(ctx, received.Add(p.cfg.RequestDeadline))
	r := &run{
		ctx:      ctx,
		cancel:   cancel,
		kind:     kind,
		id:       id.String(),
		uuid:     id,
		sink:     sink,
		received: received,
		timings:  NewTimings(),
		record: &journal.Request{
			RequestID:     id.String(),
			SessionID:     sessionID,
			Kind:          kind,
			DescriberMode: string(p.deps.Describer.Mode()),
			CreatedAt:     received.UTC(),
		},
	}
	r.timings.Set(StageQueue, time.Since(received))
	return r
}

// respond describes the scene and sends the answer: answer.start, then answer.text and
// the audio of every sentence, then answer.end (PROTO-6). The answer is limited by the
// sentence stream of PLT-7; the language model stream is closed at the limit.
func (p *Pipeline) respond(r *run, in *describer.Input, kind protocol.AnswerKind) {
	objects := p.sceneObjects(in.Objects, in.Focus)
	sentences := NewSentences(p.cfg.MaxAnswerChars)
	speech := newSpeech(r.ctx, p.deps.Synthesizer, r.id, r.timings)

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		p.emit(r, speech, kind)
	}()

	llmStarted := time.Now()
	say := func(texts []string) {
		for _, text := range texts {
			if speech.Count() == 0 && p.deps.Describer.Mode() != env.DescriberModeTemplate {
				r.timings.SetOnce(StageLLMFirstSentence, time.Since(llmStarted))
			}
			logger.Debug("sentence ready: request_id=%s seq=%d text=%s", r.id, speech.Count(), text)
			speech.Say(text)
		}
	}
	onText := func(text string) error {
		ready, done := sentences.Push(text)
		say(ready)
		if done {
			return llm.ErrStop
		}
		return nil
	}

	result, err := p.deps.Describer.Describe(r.ctx, in, onText)
	if result != nil && result.LLM != nil {
		p.llmTimings(r, result.LLM)
	}
	if err == nil {
		say(sentences.Flush())
	}
	speech.Close()
	<-emitted
	if r.cancelled() || r.rejected() {
		return
	}

	switch {
	case r.ctx.Err() != nil && !r.answerStarted():
		r.fail(protocol.NewError(protocol.CodeUpstreamTimeout, errDeadlineExceeded), context.Cause(r.ctx))
		return
	case err != nil && !r.answerStarted():
		r.fail(protocol.NewError(protocol.CodeInternal, errDescribeFailed), err)
		return
	case err != nil:
		logger.Warning("scene description stopped after the first sentence: request_id=%s error=%v", r.id, err)
	}
	if !r.answerStarted() {
		r.fail(protocol.NewError(protocol.CodeInternal, errDescribeFailed), errors.New(errNoSentences))
		return
	}

	hasHazard := false
	if result != nil {
		hasHazard = result.Hazard
	}
	p.end(r, objects, hasHazard)
}

// emit sends the sentences in order: answer.start before the first one, then for every
// sentence answer.text and its AUDIO_OUT frames with the frame numbers going on through
// the answer (PROTO-6, PLT-7). A failed TTS leaves the answer without audio (PLT-9).
func (p *Pipeline) emit(r *run, speech *speech, kind protocol.AnswerKind) {
	for sp := range speech.Output() {
		<-sp.ready
		if r.ctx.Err() != nil {
			drain(sp)
			continue
		}

		r.mu.Lock()
		if !r.started {
			r.started = true
			var audio *protocol.AudioFormat
			if sp.failed == nil {
				format := p.cfg.AudioOut
				if sp.sampleRate > 0 {
					format.SampleRate = sp.sampleRate
				}
				audio = &format
				r.audio = true
			}
			r.sink.Send(&protocol.AnswerStart{RequestID: r.uuid, Kind: kind, Priority: protocol.PriorityNormal, Audio: audio})
		}
		r.mu.Unlock()
		if sp.failed != nil {
			p.ttsFailed(r, speech, sp.failed)
		}

		r.sink.Send(&protocol.AnswerText{RequestID: r.uuid, Seq: sp.seq, Text: sp.text})
		r.mu.Lock()
		r.sentences = append(r.sentences, sp.text)
		r.mu.Unlock()

		for chunk := range sp.chunks {
			r.mu.Lock()
			if r.audio && r.ctx.Err() == nil {
				r.sink.SendAudio(r.frames, chunk)
				if r.frames == 0 {
					r.timings.SetOnce(StageServerTTFA, time.Since(r.received))
				}
				r.frames++
			}
			r.mu.Unlock()
		}
		if sp.broken != nil {
			p.ttsFailed(r, speech, sp.broken)
		}
	}
}

// ttsFailed turns the audio off for the rest of the answer: degraded_reason tts_unavailable (PLT-9).
// The end of the request itself is not a failure of TTS.
func (p *Pipeline) ttsFailed(r *run, speech *speech, err error) {
	if r.ctx.Err() != nil || errors.Is(err, errStopSpeech) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.audio || r.degradedReason == protocol.DegradedNone {
		logger.Warning("speech synthesis failed, the answer goes on without audio: request_id=%s error=%v", r.id, err)
	}
	r.audio = false
	r.degradedReason = protocol.DegradedTTSUnavailable
	speech.Stop()
}

// end sends answer.end with the whole text, the objects and the timings.
func (p *Pipeline) end(r *run, objects []protocol.SceneObject, hasHazard bool) {
	r.mu.Lock()
	text := strings.Join(r.sentences, " ")
	reason := r.degradedReason
	r.mu.Unlock()

	r.status = StatusOK
	if reason != protocol.DegradedNone {
		r.status = StatusDegraded
	}
	r.record.AnswerText = text
	r.record.Hazard = hasHazard
	r.record.DegradedReason = string(reason)
	if encoded, err := json.Marshal(objects); err == nil {
		r.record.Objects = encoded
	}

	r.timings.Set(StageServerTotal, time.Since(r.received))
	r.sink.Send(&protocol.AnswerEnd{
		RequestID:      r.uuid,
		Text:           text,
		Intent:         r.intent,
		Hazard:         hasHazard,
		Degraded:       reason != protocol.DegradedNone,
		DegradedReason: reason,
		Objects:        objects,
		Timings:        r.timings.Protocol(),
	})
	logger.Info("request answered: request_id=%s kind=%s status=%s sentences=%d server_total_ms=%.1f",
		r.id, r.kind, r.status, len(r.sentences), float64(protocol.DurationMillis(time.Since(r.received))))
	logger.Debug("answer text: request_id=%s text=%s", r.id, text)
}

// fail sends the error of the request unless it is cancelled; cause goes to the log only.
func (r *run) fail(protocolErr *protocol.Error, cause error) {
	if errors.Is(context.Cause(r.ctx), context.DeadlineExceeded) && protocolErr.Code != protocol.CodeUpstreamTimeout {
		protocolErr = protocol.NewError(protocol.CodeUpstreamTimeout, errDeadlineExceeded)
	}
	r.status = StatusError
	r.errorCode = protocolErr.Code
	if r.cancelled() || r.rejected() {
		return
	}
	logger.Warning("request failed: request_id=%s kind=%s code=%s error=%v", r.id, r.kind, protocolErr.Code, cause)
	r.sink.Send(protocol.NewErrorMessage(&r.uuid, protocolErr))
}

// answerStarted reports whether answer.start is sent.
func (r *run) answerStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.started
}

// cancelled reports whether the application cancelled the request.
func (r *run) cancelled() bool {
	return errors.Is(context.Cause(r.ctx), ErrCancelled)
}

// rejected reports whether the session ended the request with an error of its own.
func (r *run) rejected() bool {
	return errors.Is(context.Cause(r.ctx), ErrRejected)
}

// llmTimings records the stages of the language model (PLT-14).
func (p *Pipeline) llmTimings(r *run, result *llm.Result) {
	if result.TTFT > 0 {
		r.timings.Set(StageLLMTTFT, result.TTFT)
	}
	r.timings.Set(StageLLMTotal, result.Duration)
	r.record.LLMModel = result.Model
	r.record.LLMSystemFingerprint = result.SystemFingerprint
}

// finish counts the request in the metrics and gives it to the journal.
func (p *Pipeline) finish(r *run) {
	switch {
	case r.cancelled():
		r.status = StatusCancelled
		logger.Info("request cancelled: request_id=%s kind=%s", r.id, r.kind)
	case r.rejected():
		r.status = StatusError
	}
	if r.status == "" {
		r.status = StatusError
	}
	p.deps.Metrics.Requests.WithLabelValues(r.kind, r.status).Inc()
	if r.status != StatusCancelled {
		r.timings.Observe(p.deps.Metrics, r.kind)
	}

	r.record.Intent = string(r.intent)
	r.record.Status = r.status
	r.record.ErrorCode = string(r.errorCode)
	r.record.Timings = r.timings.Milliseconds()
	if current := p.deps.Config.Current(); current != nil {
		r.record.ConfigID = current.ConfigID
	}
	p.deps.Recorder.Write(r.record)
}

// sceneObjects returns the objects of answer.end in focus, in the order of the description:
// dangers first, then notices (PLT-5), then near before far and by decreasing area.
// hazard is true only for the level danger.
func (p *Pipeline) sceneObjects(objects []detector.Object, focus protocol.Focus) []protocol.SceneObject {
	type ranked struct {
		object detector.Object
		level  hazard.Level
	}
	items := make([]ranked, 0, len(objects))
	for _, object := range objects {
		if focus == protocol.FocusAhead && object.Direction != detector.DirectionAhead {
			continue
		}
		item := ranked{object: object}
		if match, ok := p.deps.Policy.Match(object); ok {
			item.level = match.Level
		}
		items = append(items, item)
	}

	rank := func(level hazard.Level) int {
		switch level {
		case hazard.LevelDanger:
			return 0
		case hazard.LevelNotice:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank(a.level) != rank(b.level) {
			return rank(a.level) < rank(b.level)
		}
		if a.object.Distance != b.object.Distance {
			return a.object.Distance == detector.DistanceNear
		}
		return a.object.Area > b.object.Area
	})

	result := make([]protocol.SceneObject, len(items))
	for i, item := range items {
		result[i] = protocol.SceneObject{
			Label:     item.object.Label,
			LabelRU:   item.object.LabelRU,
			Direction: protocol.Direction(item.object.Direction),
			Distance:  protocol.Distance(item.object.Distance),
			Hazard:    item.level == hazard.LevelDanger,
		}
	}
	return result
}

// drain reads the rest of the audio of a sentence that is not sent.
func drain(sp *spoken) {
	for range sp.chunks {
	}
}
