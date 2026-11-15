// Package app assembles the whole gateway from the configuration: the clients of
// the model services and of the language model provider, the vocabulary, the
// hazard rules, the describer, the pipelines, the sessions, the journal, the
// configuration snapshot and the HTTP server with /health, /ready, /metrics,
// /v1/session, /v1/stats and /v1/config.
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
	"github.com/nikarpoff/assistant-low-vision-platform/internal/handlers/confighandler"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/handlers/readyhandler"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/handlers/statshandler"
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
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
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
	asr       *asr.Client
	tts       *tts.Client
	llm       *llm.Client
	prompt    *describer.Prompt
	rules     *hazard.Rules
	intents   *intent.Matcher
	history   *history.Store
	collector *snapshot.Collector
	journal   *journal.Writer
	repo      *journal.Repository
	recorder  *journalRecorder
	provider  *providerProbe
	loader    *vocabulary.Loader
	pipelines *readyPipelines
	sessions  *session.Server
	http      *httpserver.Server

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	ready    chan struct{}
	stopOnce sync.Once
}

// recorder takes the records of the journal.
type recorder interface {
	Write(record journal.Record)
}

// NewApp checks the configuration files and creates the gateway. Nothing is called yet: Start does it.
func NewApp(vars *env.Vars) (*App, error) {
	return newApp(vars, nil)
}

// newApp creates the gateway; observer, if not nil, gets a copy of every journal record (tests).
func newApp(vars *env.Vars, observer recorder) (*App, error) {
	a := &App{
		vars:     vars,
		metrics:  metrics.NewMetrics(),
		provider: &providerProbe{},
		ready:    make(chan struct{}),
	}

	var err error
	if a.detector, err = detector.NewClient(vars, a.metrics); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	if a.asr, err = asr.NewClient(vars, a.metrics); err != nil {
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
		a.provider = newProviderProbe(a.llm)
		if a.prompt, err = describer.LoadPrompt(vars.AppPrompt); err != nil {
			return nil, fmt.Errorf(errBuildFormat, err)
		}
		promptHash = a.prompt.Hash()
	}

	if a.rules, err = hazard.LoadRules(vars.AppHazardRulesPath); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	if a.intents, err = intent.LoadMatcher(vars.AppIntentsPath); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	a.history, err = history.NewStore(vars.AppHistoryScenes, vars.AppHistoryTurns, time.Duration(vars.AppHistoryTTLS)*time.Second, time.Now)
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}

	a.collector, err = snapshot.NewCollector(vars, snapshot.Hashes{
		Prompt:      promptHash,
		HazardRules: a.rules.Hash(),
		Intents:     a.intents.Hash(),
	}, snapshot.Sources{Detector: a.detector, ASR: a.asr, TTS: a.tts})
	if err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}

	if a.journal, a.repo, err = journal.Open(vars, a.metrics.JournalDropped); err != nil {
		return nil, fmt.Errorf(errBuildFormat, err)
	}
	a.recorder = &journalRecorder{writer: a.journal, observer: observer}

	a.loader = vocabulary.NewLoader(a.detector, vocabulary.DefaultInitialPause, vocabulary.DefaultMaxPause)
	a.pipelines = &readyPipelines{metrics: a.metrics}
	a.sessions = session.NewServer(session.NewConfig(vars, Version), a.pipelines, a.collector, a.recorder, a.metrics)

	ready := readyhandler.NewHandler(readyhandler.Dependencies{
		Detector:         a.detector,
		ASR:              a.asr,
		TTS:              a.tts,
		LLM:              a.provider,
		Journal:          a.journal,
		Vocabulary:       a.loader,
		DetectorRequired: vars.AppDescriberMode != env.DescriberModeVLM,
	})
	a.http = httpserver.NewServer(vars, httpserver.Handlers{
		Sessions: a.sessions,
		Ready:    http.HandlerFunc(ready.GetReady),
		Metrics:  a.metrics.Handler(),
		Stats:    http.HandlerFunc(statshandler.NewHandler(stats.NewService(a.repo)).GetStats),
		Config:   http.HandlerFunc(confighandler.NewHandler(a.collector).GetConfig),
	})

	return a, nil
}

// Start starts the journal writer and collects the first configuration snapshot, then
// in the background loads the vocabulary of the detector (PLT-16), checks the provider
// and every APP_CONFIG_REFRESH_S refreshes the snapshot and checks the provider again
// (PLT-12, PLT-13, PLT-15). Every snapshot goes to the journal. The pipelines work once
// the vocabulary is loaded.
func (a *App) Start(ctx context.Context) error {
	ctx, a.cancel = context.WithCancel(ctx)
	a.journal.Start()

	if err := a.refreshSnapshot(ctx); err != nil {
		return err
	}

	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		a.refresh(ctx)
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
// requests, then the code 1001), stops the HTTP server and the background work, then
// writes the buffered journal records and closes the journal.
func (a *App) Stop() {
	a.stopOnce.Do(func() {
		a.sessions.Shutdown(context.Background())
		a.http.Stop()
		if a.cancel != nil {
			a.cancel()
		}
		a.wg.Wait()
		a.journal.Stop()
		a.repo.Close()
	})
}

// refresh checks the provider, then every APP_CONFIG_REFRESH_S refreshes the snapshot and
// checks the provider again until ctx is done.
func (a *App) refresh(ctx context.Context) {
	a.provider.check(ctx)

	ticker := time.NewTicker(time.Duration(a.vars.AppConfigRefreshS) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.refreshSnapshot(ctx); err != nil {
				logger.Error(err)
			}
			a.provider.check(ctx)
		}
	}
}

// refreshSnapshot collects the configuration snapshot and saves it in the journal (PLT-11, PLT-12).
func (a *App) refreshSnapshot(ctx context.Context) error {
	current, err := a.collector.Refresh(ctx)
	if err != nil {
		return err
	}
	a.recorder.Write(&journal.ConfigSnapshot{
		ConfigID:    current.ConfigID,
		Config:      current.Config,
		CollectedAt: current.CollectedAt,
	})
	return nil
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
		Recognizer:  a.asr,
		Intents:     a.intents,
		Synthesizer: a.tts,
		Describer:   sceneDescriber,
		History:     a.history,
		Policy:      policy,
		Recorder:    a.recorder,
		Config:      a.collector,
		Metrics:     a.metrics,
	}))
	close(a.ready)
	logger.Info("Gateway ready: describer_mode=%s vocabulary_hash=%s", a.vars.AppDescriberMode, vocab.Hash())
	return nil
}

// journalRecorder gives the records to the journal writer and, in the tests, to the observer.
type journalRecorder struct {
	writer   *journal.Writer
	observer recorder
}

// Write puts the record in the journal buffer without waiting.
func (r *journalRecorder) Write(record journal.Record) {
	r.writer.Write(record)
	if r.observer != nil {
		r.observer.Write(record)
	}
}

// providerProbe keeps the result of the last GET <APP_LLM_URL>/models: the llm state of /ready
// (PLT-13). The checks also keep the connection to the provider warm (PLT-15).
type providerProbe struct {
	client *llm.Client // nil in the template mode
	state  atomic.Value
}

func newProviderProbe(client *llm.Client) *providerProbe {
	return &providerProbe{client: client}
}

// State returns "ok" or "down" by the last check ("down" before the first one), "disabled" in the template mode.
func (p *providerProbe) State() string {
	if p.client == nil {
		return journal.StateDisabled
	}
	if state, ok := p.state.Load().(string); ok {
		return state
	}
	return journal.StateDown
}

// check asks the provider for /models; a change of the state is logged.
func (p *providerProbe) check(ctx context.Context) {
	if p.client == nil {
		return
	}

	state := journal.StateOK
	err := p.client.Models(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		state = journal.StateDown
	}

	previous := p.state.Swap(state)
	switch {
	case err != nil && previous != state:
		logger.Warning("language model provider is down: error=%v", err)
	case err != nil:
		logger.Debug("language model provider is still down: error=%v", err)
	case previous != state:
		logger.Info("Language model provider is up")
	}
}

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
