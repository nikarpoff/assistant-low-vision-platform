package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

// Texts of the errors, shared by the code and the tests.
const (
	errExtraBodyFormat   = "invalid APP_LLM_EXTRA_BODY: a JSON object is required: %w"
	errTemperatureFormat = "invalid APP_LLM_TEMPERATURE: a finite number is required: value=%v"
	errRefreshFormat     = "invalid APP_CONFIG_REFRESH_S: a positive number of seconds is required: value=%d"
	errLLMURLFormat      = "invalid APP_LLM_URL: %w"
	errEncodeFormat      = "failed to encode configuration snapshot: %w"
	errTrailingData      = "unexpected data after the JSON object"
)

//go:generate moq -out info_source_moq_test.go . InfoSource

// InfoSource gives the configuration of a model service (GET /v1/info).
type InfoSource interface {
	Info(ctx context.Context) (*upstream.Info, error)
}

// Sources are the model services whose /v1/info goes into the snapshot; a nil source is left out.
type Sources struct {
	Detector InfoSource
	ASR      InfoSource
	TTS      InfoSource
}

// Hashes are the hashes of the files the gateway loaded at start: the prompt template,
// the hazard rules and the intent phrases. The packages that load the files give them.
type Hashes struct {
	Prompt      string
	HazardRules string
	Intents     string
}

// Collector collects the snapshot at start and every APP_CONFIG_REFRESH_S (PLT-12).
//
// A service that does not answer keeps its last known /v1/info, so a short
// outage does not change config_id; until the first answer it is null.
type Collector struct {
	sources  Sources
	base     Config
	interval time.Duration
	now      func() time.Time

	refreshMu sync.Mutex
	mu        sync.RWMutex
	services  Services
	current   *Snapshot
}

// NewCollector prepares the own settings of the gateway for the snapshot.
func NewCollector(vars *env.Vars, hashes Hashes, sources Sources) (*Collector, error) {
	if vars.AppConfigRefreshS <= 0 {
		return nil, fmt.Errorf(errRefreshFormat, vars.AppConfigRefreshS)
	}
	if math.IsNaN(vars.AppLLMTemperature) || math.IsInf(vars.AppLLMTemperature, 0) {
		return nil, fmt.Errorf(errTemperatureFormat, vars.AppLLMTemperature)
	}
	extraBody, err := parseExtraBody(vars.AppLLMExtraBody)
	if err != nil {
		return nil, fmt.Errorf(errExtraBodyFormat, err)
	}

	base := Config{
		Placement:     vars.AppPlacement,
		DescriberMode: string(vars.AppDescriberMode),
		Prompt: Prompt{
			Name: vars.AppPrompt,
			Hash: hashes.Prompt,
		},
		Generation: Generation{
			Temperature: vars.AppLLMTemperature,
			Seed:        vars.AppLLMSeed,
			MaxTokens:   vars.AppLLMMaxTokens,
			ExtraBody:   extraBody,
		},
		TimeoutsMS: Timeouts{
			Detect:          vars.AppDetectTimeoutMS,
			ASR:             vars.AppASRTimeoutMS,
			LLMTTFT:         vars.AppLLMTTFTTimeoutMS,
			LLMTotal:        vars.AppLLMTotalTimeoutMS,
			TTSFirstChunk:   vars.AppTTSFirstChunkTimeoutMS,
			RequestDeadline: vars.AppRequestDeadlineMS,
			PassiveDeadline: vars.AppPassiveDeadlineMS,
		},
		HazardRulesHash: hashes.HazardRules,
		IntentsHash:     hashes.Intents,
		TTSVoice:        vars.AppTTSVoice,
		Image: Image{
			MaxSide:     vars.AppImageMaxSide,
			JPEGQuality: vars.AppJPEGQuality,
		},
	}

	// The language model is not used in the template mode and is not part of its snapshot.
	if vars.AppDescriberMode != env.DescriberModeTemplate {
		providerURL, err := providerURL(vars.AppLLMURL)
		if err != nil {
			return nil, fmt.Errorf(errLLMURLFormat, err)
		}
		base.LLM = &LLM{
			URL:   providerURL,
			Model: activeModel(vars),
		}
	}

	return &Collector{
		sources:  sources,
		base:     base,
		interval: time.Duration(vars.AppConfigRefreshS) * time.Second,
		now:      time.Now,
	}, nil
}

// Refresh collects /v1/info of the services and builds a new snapshot.
func (c *Collector) Refresh(ctx context.Context) (*Snapshot, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	services := c.services
	previous := c.current
	c.mu.RUnlock()

	c.fetch(ctx, &services)

	config := c.base
	config.Services = services
	canonical, err := canonicalJSON(&config)
	if err != nil {
		return nil, fmt.Errorf(errEncodeFormat, err)
	}

	snapshot := &Snapshot{
		ConfigID:    configID(canonical),
		CollectedAt: c.now().UTC().Truncate(time.Millisecond),
		Config:      canonical,
	}

	c.mu.Lock()
	c.services = services
	c.current = snapshot
	c.mu.Unlock()

	if previous == nil || previous.ConfigID != snapshot.ConfigID {
		logger.Info("Configuration snapshot changed: config_id=%s placement=%s describer_mode=%s",
			snapshot.ConfigID, config.Placement, config.DescriberMode)
	} else {
		logger.Debug("configuration snapshot refreshed: config_id=%s", snapshot.ConfigID)
	}

	return snapshot, nil
}

// Current returns the last collected snapshot or nil before the first Refresh.
func (c *Collector) Current() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.current
}

// Run refreshes the snapshot every APP_CONFIG_REFRESH_S until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.Refresh(ctx); err != nil {
				logger.Error(err)
			}
		}
	}
}

// fetch asks the services for /v1/info at once and replaces the infos of those that answered.
func (c *Collector) fetch(ctx context.Context, services *Services) {
	targets := []struct {
		name   string
		source InfoSource
		info   **upstream.Info
	}{
		{upstream.NameDetector, c.sources.Detector, &services.Detector},
		{upstream.NameASR, c.sources.ASR, &services.ASR},
		{upstream.NameTTS, c.sources.TTS, &services.TTS},
	}

	var wg sync.WaitGroup
	for _, target := range targets {
		if target.source == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()

			info, err := target.source.Info(ctx)
			if err != nil {
				logger.Warning("failed to refresh service info, keeping the last known: service=%s error=%v", target.name, err)
				return
			}
			*target.info = info
		}()
	}
	wg.Wait()
}

// parseExtraBody parses APP_LLM_EXTRA_BODY; an empty value is an empty object.
func parseExtraBody(raw string) (map[string]any, error) {
	extraBody := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return extraBody, nil
	}

	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&extraBody); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New(errTrailingData)
	}
	return extraBody, nil
}

// providerURL keeps the scheme, host and path of the provider address: no credentials, query or fragment.
func providerURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	provider := &url.URL{
		Scheme: strings.ToLower(parsed.Scheme),
		Host:   strings.ToLower(parsed.Host),
		Path:   strings.TrimRight(parsed.Path, "/"),
	}
	return provider.String(), nil
}

// activeModel returns the model of the describer mode: APP_VLM_MODEL in the vlm mode if it is set, else APP_LLM_MODEL.
func activeModel(vars *env.Vars) string {
	if vars.AppDescriberMode == env.DescriberModeVLM && vars.AppVLMModel != "" {
		return vars.AppVLMModel
	}
	return vars.AppLLMModel
}
