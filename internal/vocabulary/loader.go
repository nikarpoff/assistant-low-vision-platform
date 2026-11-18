package vocabulary

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// Default pauses between the attempts to load the vocabulary.
const (
	DefaultInitialPause = 500 * time.Millisecond
	DefaultMaxPause     = 10 * time.Second
)

// errNotLoadedFormat is the text of the error of Run stopped by its context.
const errNotLoadedFormat = "vocabulary not loaded: %w"

//go:generate moq -out source_moq_test.go . source

// source gives the vocabulary of the detector (GET /v1/vocabulary).
type source interface {
	Vocabulary(ctx context.Context) (*Document, error)
}

// Loader loads the vocabulary at start and keeps it (PLT-16).
// Until the vocabulary is loaded, Vocabulary returns nil and /ready answers 503.
type Loader struct {
	source       source
	initialPause time.Duration
	maxPause     time.Duration
	wait         func(ctx context.Context, pause time.Duration) error
	vocabulary   atomic.Pointer[Vocabulary]
}

// NewLoader creates a loader that retries with a pause growing twice from initialPause up to maxPause.
// Both pauses must be positive.
func NewLoader(source source, initialPause, maxPause time.Duration) *Loader {
	return &Loader{
		source:       source,
		initialPause: initialPause,
		maxPause:     maxPause,
		wait:         sleep,
	}
}

// Run loads the vocabulary, repeating failed attempts with a growing pause, and returns it.
// It returns an error only when ctx is done before the vocabulary is loaded.
func (l *Loader) Run(ctx context.Context) (*Vocabulary, error) {
	pause := l.initialPause
	for attempt := 1; ; attempt++ {
		vocabulary, err := l.load(ctx)
		if err == nil {
			l.vocabulary.Store(vocabulary)
			logger.Info("Vocabulary loaded: hash=%s classes=%d attempts=%d", vocabulary.Hash(), len(vocabulary.classes), attempt)
			return vocabulary, nil
		}

		logger.Warning("failed to load vocabulary, retrying: attempt=%d pause=%s error=%v", attempt, pause, err)
		if err := l.wait(ctx, pause); err != nil {
			return nil, fmt.Errorf(errNotLoadedFormat, err)
		}
		pause = min(2*pause, l.maxPause)
	}
}

// Vocabulary returns the loaded vocabulary or nil before it is loaded.
func (l *Loader) Vocabulary() *Vocabulary {
	return l.vocabulary.Load()
}

// Loaded reports whether the vocabulary is loaded.
func (l *Loader) Loaded() bool {
	return l.vocabulary.Load() != nil
}

// load makes one attempt: gets the document and checks it.
func (l *Loader) load(ctx context.Context) (*Vocabulary, error) {
	document, err := l.source.Vocabulary(ctx)
	if err != nil {
		return nil, err
	}
	return NewVocabulary(document)
}

// sleep waits for the pause or until ctx is done.
func sleep(ctx context.Context, pause time.Duration) error {
	timer := time.NewTimer(pause)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
