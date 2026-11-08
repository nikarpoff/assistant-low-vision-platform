// Package app assembles the whole gateway from the configuration: the clients of
// the model services and of the language model provider, the vocabulary, the
// hazard rules, the describer, the pipelines, the sessions and the HTTP server.
// cmd/gateway only runs it, and the integration tests run the real gateway in-process.
package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/endpoints/httpserver"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/intent"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/journal"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/session"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/asr"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/tts"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Version is the version of the gateway: server_version of welcome.
const Version = "0.1.0"

// Texts of the errors, shared by the code and the tests.
const (
	errNotReady          = "gateway is not ready: the vocabulary of the detector is not loaded"
	errBuildFormat       = "failed to build gateway: %w"
	errBuildDescriberFmt = "failed to build describer: %w"
)

// App is the assembled gateway.
type App struct {
	vars      *env.Vars
	metrics   *metrics.Metrics
	detector  *detector.Client
	tts       *tts.Client
	llm       *llm.Client
	prompt    *describer.Prompt
	rules     *hazard.Rules
	history   *history.Store
	collector *snapshot.Collector
	loader    *vocabulary.Loader
	pipelines *readyPipelines
	sessions  *session.Server
	http      *httpserver.Server

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	ready    chan struct{}
	stopOnce sync.Once
}

// NewApp checks the configuration files and creates the gateway. Nothing is called yet: Start does it.
func NewApp(vars *env.Vars) (*App, error) {
	a := &App{
		vars:    vars,
		metrics: metrics.NewMetrics(),
		ready:   make(chan struct{}),
	}

	var err error
	if a.detector, err = detector.NewClient(vars, a.metrics); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	asrClient, err := asr.NewClient(vars, a.metrics)
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	if a.tts, err = tts.NewClient(vars, a.metrics); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}

	promptHash := ""
	if vars.AppDescriberMode != env.DescriberModeTemplate {
		if a.llm, err = llm.NewClient(llm.NewConfig(vars), a.metrics); err != nil {
			return nil, fmt.Errorf(errBuildFormat, err)
		}
		if a.prompt, err = describer.LoadPrompt(vars.AppPrompt); err != nil {
			return nil, fmt.Errorf(errBuildFormat, err)
		}
		promptHash = a.prompt.Hash()
	}

	if a.rules, err = hazard.LoadRules(vars.AppHazardRulesPath); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	intents, err := intent.LoadMatcher(vars.AppIntentsPath)
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	a.history, err = history.NewStore(vars.AppHistoryScenes, vars.AppHistoryTurns, time.Duration(vars.AppHistoryTTLS)*time.Second, time.Now)
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}

	a.collector, err = snapshot.NewCollector(vars, snapshot.Hashes{
		Prompt:      promptHash,
		HazardRules: a.rules.Hash(),
		Intents:     intents.Hash(),
	}, snapshot.Sources{Detector: a.detector, ASR: asrClient, TTS: a.tts})
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}

	a.loader = vocabulary.NewLoader(a.detector, vocabulary.DefaultInitialPause, vocabulary.DefaultMaxPause)
	a.pipelines = &readyPipelines{metrics: a.metrics}
	a.sessions = session.NewServer(session.NewConfig(vars, Version), a.pipelines, a.collector, a.metrics)
	a.http = httpserver.NewServer(vars, a.sessions)

	return a, nil
}

// Start collects the first configuration snapshot, then loads the vocabulary of the
// detector in the background (PLT-16) and refreshes the snapshot every
// APP_CONFIG_REFRESH_S (PLT-12). The pipelines work once the vocabulary is loaded.
func (a *App) Start(ctx context.Context) error {
	ctx, a.cancel = context.WithCancel(ctx)

	if _, err := a.collector.Refresh(ctx); err != nil {
		return err
	}

	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		a.collector.Run(ctx)
	}()
	go func() {
		defer a.wg.Done()
		// A stop before the vocabulary is loaded is not an error.
		if err := a.buildPipelines(ctx); err != nil && ctx.Err() == nil {
			logger.Error(err)
		}
	}()

	return nil
}

// Run serves HTTP and the sessions until Stop.
func (a *App) Run() error {
	return a.http.Run()
}

// Stop closes the sessions as PLT-1 requires (no new ones, up to 5 s for the active
// requests, then the code 1001), stops the HTTP server and the background work.
func (a *App) Stop() {
	a.stopOnce.Do(func() {
		a.sessions.Shutdown(context.Background())
		a.http.Stop()
		if a.cancel != nil {
			a.cancel()
		}
		a.wg.Wait()
	})
}

// Handler returns the HTTP handler of the gateway.
func (a *App) Handler() http.Handler {
	return a.http.Handler()
}

// Ready is closed when the vocabulary is loaded and the pipelines work.
func (a *App) Ready() <-chan struct{} {
	return a.ready
}

// Metrics returns the metrics of the gateway.
func (a *App) Metrics() *metrics.Metrics {
	return a.metrics
}

// buildPipelines waits for the vocabulary and builds the hazard policy, the describer and the pipelines.
func (a *App) buildPipelines(ctx context.Context) error {
	vocab, err := a.loader.Run(ctx)
	if err != nil {
		return err
	}
	policy, err := hazard.NewPolicy(a.rules, vocab)
	if err != nil {
		return fmt.Errorf(errBuildFormat, err)
	}

	var completer describer.Completer
	if a.llm != nil {
		completer = a.llm
	}
	sceneDescriber, err := describer.NewDescriber(describer.NewConfig(a.vars), vocab, policy, a.prompt, completer)
	if err != nil {
		return fmt.Errorf(errBuildDescriberFmt, err)
	}

	a.pipelines.set(pipeline.NewPipeline(pipeline.NewConfig(a.vars), pipeline.Dependencies{
		Detector:    a.detector,
		Synthesizer: a.tts,
		Describer:   sceneDescriber,
		History:     a.history,
		Policy:      policy,
		Recorder:    discardRecorder{},
		Config:      a.collector,
		Metrics:     a.metrics,
	}))
	close(a.ready)
	logger.Info("Gateway ready: describer_mode=%s vocabulary_hash=%s", a.vars.AppDescriberMode, vocab.Hash())
	return nil
}

// discardRecorder is the journal until it is connected: the records are dropped.
type discardRecorder struct{}

// Write drops the record.
func (discardRecorder) Write(journal.Record) {}

// readyPipelines runs the requests once the pipelines are built; until then a request
// gets the error busy (the vocabulary of the detector is not loaded, PLT-16).
type readyPipelines struct {
	metrics  *metrics.Metrics
	pipeline atomic.Pointer[pipeline.Pipeline]
}

func (r *readyPipelines) set(p *pipeline.Pipeline) {
	r.pipeline.Store(p)
}

// Describe runs the describe pipeline.
func (r *readyPipelines) Describe(ctx context.Context, req *pipeline.DescribeRequest, sink pipeline.Sink) {
	if p := r.pipeline.Load(); p != nil {
		p.Describe(ctx, req, sink)
		return
	}
	r.notReady(pipeline.KindDescribe, req.RequestID, sink)
}

// Ask runs the question pipeline.
func (r *readyPipelines) Ask(ctx context.Context, req *pipeline.AskRequest, sink pipeline.Sink) {
	if p := r.pipeline.Load(); p != nil {
		p.Ask(ctx, req, sink)
		return
	}
	r.notReady(pipeline.KindAsk, req.RequestID, sink)
}

// Passive runs the passive pipeline.
func (r *readyPipelines) Passive(ctx context.Context, req *pipeline.PassiveRequest, sink pipeline.Sink) {
	if p := r.pipeline.Load(); p != nil {
		p.Passive(ctx, req, sink)
		return
	}
	r.notReady(pipeline.KindPassive, req.RequestID, sink)
}

func (r *readyPipelines) notReady(kind string, id uuid.UUID, sink pipeline.Sink) {
	r.metrics.Requests.WithLabelValues(kind, pipeline.StatusError).Inc()
	sink.Send(protocol.NewErrorMessage(&id, protocol.NewError(protocol.CodeBusy, errNotReady)))
}
