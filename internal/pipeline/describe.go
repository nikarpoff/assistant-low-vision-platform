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
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/asr"
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

//go:generate moq -out history_store_moq_test.go . HistoryStore

// HistoryStore keeps the history of a device (*history.Store, PLT-10).
type HistoryStore interface {
	Get(deviceID string) history.History
	AddScene(deviceID string, scene history.Scene)
	AddTurn(deviceID string, turns ...history.Turn)
}

//go:generate moq -out speech_recognizer_moq_test.go . SpeechRecognizer

// SpeechRecognizer transcribes the question (*asr.Client).
type SpeechRecognizer interface {
	Recognize(ctx context.Context, requestID string, pcm []byte) (*asr.RecognizeResponse, error)
}

//go:generate moq -out intent_recognizer_moq_test.go . IntentRecognizer

// IntentRecognizer gives the intent of the transcript (*intent.Matcher, PLT-4).
type IntentRecognizer interface {
	Recognize(text string) (protocol.Intent, error)
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
	// PassiveDeadline is APP_PASSIVE_DEADLINE_MS: an older passive result is stale (PROTO-8).
	PassiveDeadline time.Duration
	// PassiveNotices is APP_PASSIVE_NOTICES: the passive mode also speaks the level notice (PLT-5).
	PassiveNotices bool
	// HazardCooldown is APP_HAZARD_COOLDOWN_MS: the period of one warning by class and direction (PLT-6).
	HazardCooldown time.Duration
	// SceneFreshness is APP_SCENE_FRESHNESS_MS: a question without a snapshot uses a younger last scene (PLT-3).
	SceneFreshness time.Duration
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
		PassiveDeadline: time.Duration(vars.AppPassiveDeadlineMS) * time.Millisecond,
		PassiveNotices:  vars.AppPassiveNotices,
		HazardCooldown:  time.Duration(vars.AppHazardCooldownMS) * time.Millisecond,
		SceneFreshness:  time.Duration(vars.AppSceneFreshnessMS) * time.Millisecond,
	}
}

// Dependencies are the services and the state the pipelines use.
type Dependencies struct {
	Detector    ObjectDetector
	Recognizer  SpeechRecognizer
	Intents     IntentRecognizer
	Synthesizer Synthesizer
	Describer   SceneDescriber
	History     HistoryStore
	Policy      *hazard.Policy
	Recorder    Recorder
	Config      ConfigSource
	Metrics     *metrics.Metrics
}

// Pipeline runs the requests of all sessions. Its own state is the hazard cooldowns of
// the devices and the requests in progress of the sessions (PROTO-8).
type Pipeline struct {
	cfg  Config
	deps Dependencies
	// now is the clock of the hazard cooldowns (time.Now outside the tests).
	now func() time.Time

	mu        sync.Mutex
	cooldowns map[string]*deviceCooldown // device_id -> cooldown
	sessions  map[string]*inProgress     // session_id -> requests in progress
}

// NewPipeline creates the pipelines.
func NewPipeline(cfg Config, deps Dependencies) *Pipeline {
	return &Pipeline{
		cfg:       cfg,
		deps:      deps,
		now:       time.Now,
		cooldowns: map[string]*deviceCooldown{},
		sessions:  map[string]*inProgress{},
	}
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
	defer p.beginUser(req.SessionID)()
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
	objects, ok := p.detection(r, detected, err)
	if !ok {
		p.finish(r)
		return
	}

	in := &describer.Input{
		RequestID: r.id,
		Objects:   objects,
		Focus:     req.Focus,
		History:   p.deps.History.Get(req.DeviceID),
		Image:     req.Image,
	}
	if text, answered := p.respond(r, in, protocol.AnswerKindDescription); answered {
		p.deps.History.AddScene(req.DeviceID, history.Scene{Objects: objects, Description: text, At: req.ReceivedAt})
	}
	p.finish(r)
}

// detection returns the objects of the detector. A failed detector fails the request
// with detector_unavailable in the template and llm modes; in the vlm mode the scene is
// described from the snapshot alone, with no objects (PLT-9, DSC-6).
func (p *Pipeline) detection(r *run, detected *detector.DetectResponse, err error) ([]detector.Object, bool) {
	if err == nil {
		return detected.Objects, true
	}
	if p.deps.Describer.Mode() == env.DescriberModeVLM && r.ctx.Err() == nil {
		logger.Warning("object detector failed, the vlm mode describes the snapshot alone: request_id=%s error=%v", r.id, err)
		return nil, true
	}
	r.fail(protocol.NewError(protocol.CodeDetectorUnavailable, errDetectorUnavailable), err)
	return nil, false
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
	priority protocol.Priority
	// staleAt is when a passive result gets stale (PROTO-8); zero for a user request.
	staleAt time.Time

	mu             sync.Mutex
	started        bool // answer.start is sent
	stale          bool // the passive answer got stale before answer.start
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
		priority: protocol.PriorityNormal,
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
// sentence stream of PLT-7; the language model stream is closed at the limit. A failure
// of the provider degrades the answer by PLT-9 (degrade.go). It returns the text of the
// answer and whether answer.end is sent.
func (p *Pipeline) respond(r *run, in *describer.Input, kind protocol.AnswerKind) (string, bool) {
	objects := p.sceneObjects(in.Objects, in.Focus)
	sentences := NewSentences(p.cfg.MaxAnswerChars)
	speech := newSpeech(r.ctx, p.deps.Synthesizer, r.id, r.timings)

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		p.emit(r, speech, kind)
	}()

	llmStarted := time.Now()
	fromLLM := p.deps.Describer.Mode() != env.DescriberModeTemplate
	say := func(texts []string) {
		for _, text := range texts {
			if speech.Count() == 0 && fromLLM {
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
	switch failure := providerFailure(err); {
	case err == nil:
		say(sentences.Flush())
	case failure != nil && r.ctx.Err() == nil:
		r.record.LLMError = string(failure.Reason)
		r.degrade(degradedReason(failure))
		// The sentences the stream completed are ready; the unfinished tail is dropped
		say(sentences.Abort())
		if speech.Count() > 0 {
			logger.Warning("language model failed after the first sentence, the answer is closed with the ready sentences: request_id=%s reason=%s sentences=%d",
				r.id, failure.Reason, speech.Count())
			err = nil
			break
		}
		logger.Warning("language model failed before the first sentence, the answer is built by the template: request_id=%s reason=%s", r.id, failure.Reason)
		fromLLM = false
		sentences = NewSentences(p.cfg.MaxAnswerChars)
		if result, err = p.deps.Describer.Template(in, onText); err == nil {
			say(sentences.Flush())
		}
	}
	speech.Close()
	<-emitted
	if r.cancelled() || r.rejected() {
		return "", false
	}
	if r.isStale() {
		return "", false
	}

	switch {
	case r.ctx.Err() != nil && !r.answerStarted():
		r.fail(protocol.NewError(protocol.CodeUpstreamTimeout, errDeadlineExceeded), context.Cause(r.ctx))
		return "", false
	case err != nil && !r.answerStarted():
		r.fail(protocol.NewError(protocol.CodeInternal, errDescribeFailed), err)
		return "", false
	case err != nil:
		logger.Warning("scene description stopped after the first sentence: request_id=%s error=%v", r.id, err)
	}
	if !r.answerStarted() {
		r.fail(protocol.NewError(protocol.CodeInternal, errDescribeFailed), errors.New(errNoSentences))
		return "", false
	}

	hasHazard := false
	if result != nil {
		hasHazard = result.Hazard
	}
	return p.end(r, objects, hasHazard), true
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
		if !r.started && !r.staleAt.IsZero() && !time.Now().Before(r.staleAt) {
			// The passive result came too late: nothing of it is sent (PROTO-8)
			r.stale = true
		}
		if r.stale {
			r.mu.Unlock()
			speech.Stop()
			drain(sp)
			continue
		}
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
			r.sink.Send(&protocol.AnswerStart{RequestID: r.uuid, Kind: kind, Priority: r.priority, Audio: audio})
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
	r.degradeLocked(protocol.DegradedTTSUnavailable)
	speech.Stop()
}

// end sends answer.end with the whole text, the objects and the timings, and returns the text.
func (p *Pipeline) end(r *run, objects []protocol.SceneObject, hasHazard bool) string {
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
	return text
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

// isStale reports whether the passive answer got stale before answer.start.
func (r *run) isStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stale
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
