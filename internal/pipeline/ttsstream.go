package pipeline

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/tts"
)

// MaxParallelSynthesis is the number of TTS calls of one request at a time (PLT-7):
// the next sentence is synthesized while the previous one is sent.
const MaxParallelSynthesis = 2

// queueSize is the room for the sentences waiting for a slot of synthesis or for the emitter.
const queueSize = 16

// Texts of the speech errors, shared by the code and the tests.
const (
	errSpeechStopped = "speech synthesis stopped after a failure"
	errEmptyAudio    = "speech synthesis gave no audio"
)

// errStopSpeech is the cancellation cause of the TTS calls after a failure.
var errStopSpeech = errors.New(errSpeechStopped)

//go:generate moq -out synthesizer_moq_test.go . Synthesizer

// Synthesizer synthesizes the speech of a sentence (*tts.Client).
type Synthesizer interface {
	Synthesize(ctx context.Context, requestID, text string) (*tts.Stream, error)
}

// spoken is a sentence on its way to the application: its text and the audio as it comes.
type spoken struct {
	seq  int
	text string
	// ready is closed when the first audio chunk came or the synthesis failed.
	ready chan struct{}
	// chunks are the audio chunks; closed after the last one.
	chunks chan []byte
	// sampleRate is X-Sample-Rate of the answer of TTS, 0 if unknown; valid after ready.
	sampleRate int
	// failed is the failure before the first chunk: there is no audio at all; valid after ready.
	failed error
	// broken is the failure after the first chunk; valid after chunks are closed.
	broken error
}

// speech synthesizes the sentences in their order, at most MaxParallelSynthesis at a
// time, and gives them to the emitter in the same order (PLT-7).
type speech struct {
	ctx         context.Context
	stop        context.CancelCauseFunc
	synthesizer Synthesizer
	requestID   string
	timings     *Timings

	input  chan *spoken
	output chan *spoken
	slots  chan struct{}

	startOnce sync.Once
	startedAt time.Time // the first TTS request was sent
	sentences int
}

// newSpeech starts the synthesis of the sentences of one request.
func newSpeech(ctx context.Context, synthesizer Synthesizer, requestID string, timings *Timings) *speech {
	ctx, stop := context.WithCancelCause(ctx)
	s := &speech{
		ctx:         ctx,
		stop:        stop,
		synthesizer: synthesizer,
		requestID:   requestID,
		timings:     timings,
		input:       make(chan *spoken, queueSize),
		output:      make(chan *spoken, queueSize),
		slots:       make(chan struct{}, MaxParallelSynthesis),
	}
	go s.dispatch()
	return s
}

// Say queues the sentence for synthesis. It is called from one goroutine; the dispatcher
// takes every sentence, so the call does not block for long.
func (s *speech) Say(text string) {
	sp := &spoken{
		seq:    s.sentences,
		text:   text,
		ready:  make(chan struct{}),
		chunks: make(chan []byte, 16),
	}
	s.sentences++
	s.input <- sp
}

// Count returns the number of the sentences queued.
func (s *speech) Count() int {
	return s.sentences
}

// Close tells that no more sentences come.
func (s *speech) Close() {
	close(s.input)
}

// Stop cancels the TTS calls in progress and leaves the next sentences without audio.
func (s *speech) Stop() {
	s.stop(errStopSpeech)
}

// Output gives the sentences in their order; it is closed after the last one. The
// emitter must read it to the end, also after a cancellation.
func (s *speech) Output() <-chan *spoken {
	return s.output
}

// dispatch starts the synthesis of every sentence in order when a slot is free.
func (s *speech) dispatch() {
	defer close(s.output)

	for sp := range s.input {
		if s.ctx.Err() != nil {
			sp.fail(context.Cause(s.ctx))
			s.output <- sp
			continue
		}
		select {
		case s.slots <- struct{}{}:
		case <-s.ctx.Done():
			sp.fail(context.Cause(s.ctx))
			s.output <- sp
			continue
		}
		s.startOnce.Do(func() { s.startedAt = time.Now() })
		go s.synthesize(sp)
		s.output <- sp
	}
}

// synthesize calls TTS for the sentence and passes its audio chunks on.
func (s *speech) synthesize(sp *spoken) {
	defer func() { <-s.slots }()

	stream, err := s.synthesizer.Synthesize(s.ctx, s.requestID, sp.text)
	if err != nil {
		sp.fail(err)
		return
	}
	defer stream.Close()
	sp.sampleRate = stream.SampleRate

	first := true
	for {
		chunk, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
				if first {
					err = errors.New(errEmptyAudio)
				}
			}
			if first {
				sp.fail(err)
				return
			}
			sp.broken = err
			close(sp.chunks)
			return
		}

		elapsed := time.Since(s.startedAt)
		if first {
			first = false
			s.timings.SetOnce(StageTTSFirstChunk, elapsed)
			close(sp.ready)
		}
		s.timings.Set(StageTTSTotal, elapsed)

		select {
		case sp.chunks <- chunk:
		case <-s.ctx.Done():
			sp.broken = context.Cause(s.ctx)
			close(sp.chunks)
			return
		}
	}
}

// fail ends the sentence without audio.
func (sp *spoken) fail(err error) {
	sp.failed = err
	close(sp.ready)
	close(sp.chunks)
}
