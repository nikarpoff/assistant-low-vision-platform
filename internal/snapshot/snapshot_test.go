package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

const contractsDir = "../../testdata/contracts"

// readInfo reads /v1/info of a service from the contract examples.
func readInfo(t *testing.T, service string) *upstream.Info {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(contractsDir, service, "info.response.json"))
	require.NoError(t, err)
	info := &upstream.Info{}
	require.NoError(t, json.Unmarshal(body, info))
	return info
}

// infoSource returns a source that answers with the info.
func infoSource(info *upstream.Info) *InfoSourceMock {
	return &InfoSourceMock{
		InfoFunc: func(ctx context.Context) (*upstream.Info, error) {
			return info, nil
		},
	}
}

func TestConfigSnapshot(t *testing.T) {
	var (
		vars     *env.Vars
		hashes   Hashes
		detector *InfoSourceMock
		asr      *InfoSourceMock
		tts      *InfoSourceMock
		getLogs  func() string
	)

	setup := func(t *testing.T) {
		getLogs = logger.InitTestLogger()
		t.Setenv("APP_AUTH_DISABLED", "true")
		t.Setenv("APP_LLM_MODEL", "qwen/qwen3-30b-a3b")
		t.Setenv("APP_LLM_API_KEY", "sk-provider-key")
		var err error
		vars, err = env.Load()
		require.NoError(t, err)
		hashes = Hashes{Prompt: "5b1c0d9e8f7a", HazardRules: "0a1b2c3d4e5f", Intents: "9f8e7d6c5b4a"}
		detector = infoSource(readInfo(t, "objects-detector"))
		asr = infoSource(readInfo(t, "asr-service"))
		tts = infoSource(readInfo(t, "tts-service"))
	}

	collect := func(t *testing.T) *Snapshot {
		t.Helper()
		collector, err := NewCollector(vars, hashes, Sources{Detector: detector, ASR: asr, TTS: tts})
		require.NoError(t, err)
		snapshot, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		return snapshot
	}

	decode := func(t *testing.T, snapshot *Snapshot) map[string]any {
		t.Helper()
		config := map[string]any{}
		require.NoError(t, json.Unmarshal(snapshot.Config, &config))
		return config
	}

	t.Run("must_pass_stable", func(t *testing.T) {
		setup(t)
		collector, err := NewCollector(vars, hashes, Sources{Detector: detector, ASR: asr, TTS: tts})
		require.NoError(t, err)
		collector.now = func() time.Time { return time.Date(2026, 11, 1, 13, 0, 0, 123456789, time.FixedZone("MSK", 3*3600)) }

		first, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		second, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		other := collect(t)

		require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{12}$`), first.ConfigID)
		require.Equal(t, first.ConfigID, second.ConfigID)
		require.Equal(t, first.ConfigID, other.ConfigID)
		require.Equal(t, string(first.Config), string(other.Config))
		sum := sha256.Sum256(first.Config)
		require.Equal(t, hex.EncodeToString(sum[:])[:12], first.ConfigID)
		canonical, err := canonicalJSON(decode(t, first))
		require.NoError(t, err)
		require.Equal(t, string(canonical), string(first.Config))
		require.Equal(t, time.Date(2026, 11, 1, 10, 0, 0, 123000000, time.UTC), first.CollectedAt)
		require.Same(t, second, collector.Current())
		require.Len(t, detector.InfoCalls(), 3)
		require.JSONEq(t, `{
			"placement": "dev",
			"describer_mode": "llm",
			"services": {
				"detector": {"service": "objects-detector", "version": "0.1.0", "git_sha": "3f2c1ab", "backend": "ultralytics", "model": "yoloe-11s-seg", "device": "cuda", "warmed_up": true, "params": {"imgsz": 640, "conf": 0.35, "vocabulary_hash": "a1b2c3d4e5f6"}},
				"asr": {"service": "asr-service", "version": "0.1.0", "git_sha": "3f2c1ab", "backend": "gigaam", "model": "v2_ctc", "device": "cuda", "warmed_up": true, "params": {"language": "ru", "sample_rate": 16000, "vad_enabled": true}},
				"tts": {"service": "tts-service", "version": "0.1.0", "git_sha": "3f2c1ab", "backend": "silero", "model": "v4_ru", "device": "cpu", "warmed_up": true, "params": {"voice": "xenia", "sample_rate": 48000, "cache_max_items": 256}}
			},
			"llm": {"url": "https://polza.ai/api/v1", "model": "qwen/qwen3-30b-a3b"},
			"prompt": {"name": "default", "hash": "5b1c0d9e8f7a"},
			"generation": {"temperature": 0, "seed": 42, "max_tokens": 96, "extra_body": {}},
			"timeouts_ms": {"detect": 400, "asr": 2000, "llm_ttft": 900, "llm_total": 5000, "tts_first_chunk": 1000, "request_deadline": 8000, "passive_deadline": 1500},
			"hazard_rules_hash": "0a1b2c3d4e5f",
			"intents_hash": "9f8e7d6c5b4a",
			"tts_voice": "",
			"image": {"max_side": 640, "jpeg_quality": 80}
		}`, string(first.Config))
		require.Contains(t, getLogs(), "INFO: Configuration snapshot changed: config_id="+first.ConfigID+" placement=dev describer_mode=llm")
	})

	t.Run("must_pass_canonical_extra_body", func(t *testing.T) {
		setup(t)
		vars.AppLLMExtraBody = `{"reasoning": {"enabled": false}, "top_p": 1.0, "stop": ["<end>"]}`
		spaced := collect(t)
		vars.AppLLMExtraBody = `{"stop":["<end>"],"top_p":1.0,"reasoning":{"enabled":false}}`

		compact := collect(t)

		require.Equal(t, spaced.ConfigID, compact.ConfigID)
		require.Contains(t, string(compact.Config), `"extra_body":{"reasoning":{"enabled":false},"stop":["<end>"],"top_p":1.0}`)
	})

	t.Run("must_pass_changes_on_llm_host", func(t *testing.T) {
		setup(t)
		polza := collect(t)
		vars.AppLLMURL = "http://gpt2giga:8090/v1"
		gigachat := collect(t)
		vars.AppLLMURL = "https://polza.ai/api/v2"

		otherPath := collect(t)

		require.NotEqual(t, polza.ConfigID, gigachat.ConfigID)
		require.NotEqual(t, polza.ConfigID, otherPath.ConfigID)
		require.Equal(t, map[string]any{"url": "http://gpt2giga:8090/v1", "model": "qwen/qwen3-30b-a3b"}, decode(t, gigachat)["llm"])
	})

	t.Run("must_pass_changes_on_model", func(t *testing.T) {
		setup(t)
		qwen := collect(t)
		vars.AppLLMModel = "GigaChat-2-Max"
		gigachat := collect(t)
		vars.AppLLMModel = "qwen/qwen3-30b-a3b"
		detectorInfo := readInfo(t, "objects-detector")
		detectorInfo.Model = "yolo11n"
		detector = infoSource(detectorInfo)

		otherDetector := collect(t)

		require.NotEqual(t, qwen.ConfigID, gigachat.ConfigID)
		require.NotEqual(t, qwen.ConfigID, otherDetector.ConfigID)
		require.NotEqual(t, gigachat.ConfigID, otherDetector.ConfigID)
	})

	t.Run("must_pass_changes_on_temperature", func(t *testing.T) {
		setup(t)
		zero := collect(t)
		vars.AppLLMTemperature = 0.3

		warm := collect(t)

		require.NotEqual(t, zero.ConfigID, warm.ConfigID)
		require.Equal(t, 0.3, decode(t, warm)["generation"].(map[string]any)["temperature"])
	})

	t.Run("must_pass_changes_on_generation_parameters", func(t *testing.T) {
		setup(t)
		base := collect(t)
		ids := map[string]bool{base.ConfigID: true}
		changes := []func(){
			func() { vars.AppLLMSeed = 7 },
			func() { vars.AppLLMMaxTokens = 128 },
			func() { vars.AppLLMExtraBody = `{"reasoning": {"enabled": false}}` },
		}

		for _, change := range changes {
			setup(t)
			change()
			ids[collect(t).ConfigID] = true
		}

		require.Len(t, ids, 4)
	})

	t.Run("must_pass_changes_on_timeout", func(t *testing.T) {
		setup(t)
		base := collect(t)
		ids := map[string]bool{base.ConfigID: true}
		changes := []func(){
			func() { vars.AppDetectTimeoutMS = 500 },
			func() { vars.AppASRTimeoutMS = 2500 },
			func() { vars.AppLLMTTFTTimeoutMS = 1200 },
			func() { vars.AppLLMTotalTimeoutMS = 6000 },
			func() { vars.AppTTSFirstChunkTimeoutMS = 800 },
			func() { vars.AppRequestDeadlineMS = 9000 },
			func() { vars.AppPassiveDeadlineMS = 2000 },
		}

		for _, change := range changes {
			setup(t)
			change()
			ids[collect(t).ConfigID] = true
		}

		require.Len(t, ids, 8)
	})

	t.Run("must_pass_changes_on_placement", func(t *testing.T) {
		setup(t)
		dev := collect(t)
		vars.AppPlacement = "A"
		variantA := collect(t)
		vars.AppPlacement = "B"

		variantB := collect(t)

		require.NotEqual(t, dev.ConfigID, variantA.ConfigID)
		require.NotEqual(t, dev.ConfigID, variantB.ConfigID)
		require.NotEqual(t, variantA.ConfigID, variantB.ConfigID)
	})

	t.Run("must_pass_changes_on_own_settings", func(t *testing.T) {
		setup(t)
		base := collect(t)
		ids := map[string]bool{base.ConfigID: true}
		changes := []func(){
			func() { vars.AppDescriberMode = env.DescriberModeVLM },
			func() { vars.AppPrompt = "short" },
			func() { hashes.Prompt = "000000000001" },
			func() { hashes.HazardRules = "000000000002" },
			func() { hashes.Intents = "000000000003" },
			func() { vars.AppTTSVoice = "baya" },
			func() { vars.AppImageMaxSide = 512 },
			func() { vars.AppJPEGQuality = 70 },
		}

		for _, change := range changes {
			setup(t)
			change()
			ids[collect(t).ConfigID] = true
		}

		require.Len(t, ids, 9)
	})

	t.Run("must_pass_template_mode_without_llm", func(t *testing.T) {
		setup(t)
		llm := collect(t)
		vars.AppDescriberMode = env.DescriberModeTemplate
		template := collect(t)
		vars.AppLLMURL = "http://gpt2giga:8090/v1"
		vars.AppLLMModel = "GigaChat-2-Max"
		vars.AppVLMModel = "GigaChat-2-Max"

		otherProvider := collect(t)

		require.NotEqual(t, llm.ConfigID, template.ConfigID)
		require.Equal(t, template.ConfigID, otherProvider.ConfigID)
		config := decode(t, template)
		require.NotContains(t, config, "llm")
		require.Equal(t, "template", config["describer_mode"])
	})

	t.Run("must_pass_vlm_model", func(t *testing.T) {
		setup(t)
		vars.AppDescriberMode = env.DescriberModeVLM
		fallback := collect(t)
		vars.AppVLMModel = "qwen/qwen2.5-vl-7b-instruct"

		vlm := collect(t)

		require.Equal(t, map[string]any{"url": "https://polza.ai/api/v1", "model": "qwen/qwen3-30b-a3b"}, decode(t, fallback)["llm"])
		require.Equal(t, map[string]any{"url": "https://polza.ai/api/v1", "model": "qwen/qwen2.5-vl-7b-instruct"}, decode(t, vlm)["llm"])
		require.NotEqual(t, fallback.ConfigID, vlm.ConfigID)
	})

	t.Run("must_pass_no_secrets", func(t *testing.T) {
		setup(t)
		clean := collect(t)
		secrets := []string{
			"sk-secret-llm", "detector-secret", "asr-secret", "tts-secret", "admin-secret",
			"device-secret-1", "device-secret-2", "pg-secret", "url-secret", "query-secret",
		}
		vars.AppLLMAPIKey = "sk-secret-llm"
		vars.AppDetectorToken = "detector-secret"
		vars.AppASRToken = "asr-secret"
		vars.AppTTSToken = "tts-secret"
		vars.AppAdminToken = "admin-secret"
		vars.AppDeviceTokens = []string{"device-secret-1", "device-secret-2"}
		vars.AppPostgresDSN = "postgres://alv:pg-secret@postgres:5432/alv"
		vars.AppLLMURL = "https://user:url-secret@polza.ai/api/v1/?key=query-secret#fragment"

		withSecrets := collect(t)

		require.Equal(t, clean.ConfigID, withSecrets.ConfigID)
		require.Equal(t, string(clean.Config), string(withSecrets.Config))
		for _, secret := range secrets {
			require.NotContains(t, string(withSecrets.Config), secret)
		}
		require.Equal(t, "https://polza.ai/api/v1", decode(t, withSecrets)["llm"].(map[string]any)["url"])
	})

	t.Run("must_pass_placement_top_level", func(t *testing.T) {
		setup(t)
		vars.AppPlacement = "B"

		snapshot := collect(t)

		config := decode(t, snapshot)
		require.Equal(t, "B", config["placement"])
		body, err := json.Marshal(snapshot)
		require.NoError(t, err)
		response := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(body, &response))
		require.Len(t, response, 3)
		require.Equal(t, `"`+snapshot.ConfigID+`"`, string(response["config_id"]))
		require.Regexp(t, regexp.MustCompile(`^"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z"$`), string(response["collected_at"]))
		require.JSONEq(t, string(snapshot.Config), string(response["config"]))
	})

	t.Run("must_pass_keeps_last_known_info", func(t *testing.T) {
		setup(t)
		collector, err := NewCollector(vars, hashes, Sources{Detector: detector, ASR: asr, TTS: tts})
		require.NoError(t, err)
		first, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		detector.InfoFunc = func(ctx context.Context) (*upstream.Info, error) {
			return nil, errors.New("failed to get service info: upstream timeout: upstream=detector timeout=2s")
		}

		second, err := collector.Refresh(context.Background())

		require.NoError(t, err)
		require.Equal(t, first.ConfigID, second.ConfigID)
		require.Contains(t, getLogs(), "WARNING: failed to refresh service info, keeping the last known: service=detector error=failed to get service info: upstream timeout: upstream=detector timeout=2s")
	})

	t.Run("must_pass_service_without_info", func(t *testing.T) {
		setup(t)
		detectorInfo := detector.InfoFunc
		detector.InfoFunc = func(ctx context.Context) (*upstream.Info, error) {
			return nil, errors.New("failed to get service info: upstream unavailable: upstream=detector status=500")
		}
		collector, err := NewCollector(vars, hashes, Sources{Detector: detector, ASR: asr, TTS: tts})
		require.NoError(t, err)
		withoutDetector, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		detector.InfoFunc = detectorInfo

		withDetector, err := collector.Refresh(context.Background())

		require.NoError(t, err)
		require.Nil(t, decode(t, withoutDetector)["services"].(map[string]any)["detector"])
		require.NotNil(t, decode(t, withDetector)["services"].(map[string]any)["detector"])
		require.NotEqual(t, withoutDetector.ConfigID, withDetector.ConfigID)
		require.Equal(t, withDetector.ConfigID, collect(t).ConfigID)
	})

	t.Run("must_pass_run_refreshes", func(t *testing.T) {
		setup(t)
		collector, err := NewCollector(vars, hashes, Sources{Detector: detector, ASR: asr, TTS: tts})
		require.NoError(t, err)
		collector.interval = 5 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		go func() {
			collector.Run(ctx)
			close(done)
		}()
		require.Eventually(t, func() bool { return len(tts.InfoCalls()) >= 3 }, 2*time.Second, time.Millisecond)
		cancel()
		<-done

		require.NotNil(t, collector.Current())
		require.Equal(t, collect(t).ConfigID, collector.Current().ConfigID)
	})

	t.Run("must_fail_invalid_extra_body", func(t *testing.T) {
		setup(t)
		vars.AppLLMExtraBody = `["reasoning"]`

		collector, err := NewCollector(vars, hashes, Sources{})

		require.Nil(t, collector)
		require.EqualError(t, err, "invalid APP_LLM_EXTRA_BODY: a JSON object is required: json: cannot unmarshal array into Go value of type map[string]interface {}")
	})

	t.Run("must_fail_extra_body_trailing_data", func(t *testing.T) {
		setup(t)
		vars.AppLLMExtraBody = `{"top_p": 1} {}`

		collector, err := NewCollector(vars, hashes, Sources{})

		require.Nil(t, collector)
		require.EqualError(t, err, "invalid APP_LLM_EXTRA_BODY: a JSON object is required: unexpected data after the JSON object")
	})

	t.Run("must_fail_invalid_temperature", func(t *testing.T) {
		setup(t)
		vars.AppLLMTemperature = math.NaN()

		collector, err := NewCollector(vars, hashes, Sources{})

		require.Nil(t, collector)
		require.EqualError(t, err, "invalid APP_LLM_TEMPERATURE: a finite number is required: value=NaN")
	})

	t.Run("must_fail_invalid_refresh_interval", func(t *testing.T) {
		setup(t)
		vars.AppConfigRefreshS = 0

		collector, err := NewCollector(vars, hashes, Sources{})

		require.Nil(t, collector)
		require.EqualError(t, err, "invalid APP_CONFIG_REFRESH_S: a positive number of seconds is required: value=0")
	})

	t.Run("must_fail_invalid_llm_url", func(t *testing.T) {
		setup(t)
		vars.AppLLMURL = "http://[::1"

		collector, err := NewCollector(vars, hashes, Sources{})

		require.Nil(t, collector)
		require.EqualError(t, err, `invalid APP_LLM_URL: parse "http://[::1": missing ']' in host`)
	})
}
