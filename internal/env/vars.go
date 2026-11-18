package env

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	cenv "github.com/caarlos0/env/v6"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// DescriberMode is the way the scene description is built (DSC-1).
type DescriberMode string

// Describer modes.
const (
	DescriberModeTemplate DescriberMode = "template"
	DescriberModeLLM      DescriberMode = "llm"
	DescriberModeVLM      DescriberMode = "vlm"
)

// Texts of the configuration errors, shared by the code and the tests.
const (
	errParseFormat           = "failed to parse environment variables: %w"
	errRequiredFormat        = "%s environment variable is required unless APP_AUTH_DISABLED=true"
	errUnknownDescriberMode  = "unknown APP_DESCRIBER_MODE %q: allowed values are template, llm, vlm"
	errLLMModelRequired      = "APP_LLM_MODEL environment variable is required in llm mode: the model identifier is part of config_id"
	errVLMModelRequired      = "APP_VLM_MODEL or APP_LLM_MODEL environment variable is required in vlm mode: the model identifier is part of config_id"
	errLLMURLScheme          = "APP_LLM_URL must be an absolute http or https URL: scheme=%q host=%q"
	errLogLevelFormat        = "invalid APP_LOG_LEVEL: %w"
	warnEmptyLLMAPIKeyFormat = "APP_LLM_API_KEY is empty: requests to the language model provider are sent without a key, describer_mode=%s"
)

// Vars holds all configuration of the gateway.
type Vars struct {
	// server variables
	AppServerPort            int    `env:"APP_SERVER_PORT" envDefault:"80"`
	AppServerReadTimeout     int    `env:"APP_SERVER_READ_TIMEOUT" envDefault:"15"`
	AppServerWriteTimeout    int    `env:"APP_SERVER_WRITE_TIMEOUT" envDefault:"15"`
	AppServerIdleTimeout     int    `env:"APP_SERVER_IDLE_TIMEOUT" envDefault:"60"`
	AppServerShutdownTimeout int    `env:"APP_SERVER_SHUTDOWN_TIMEOUT" envDefault:"5"`
	AppLogLevel              string `env:"APP_LOG_LEVEL" envDefault:"INFO"`

	// authentication variables
	AppAuthDisabled bool     `env:"APP_AUTH_DISABLED" envDefault:"false"`
	AppDeviceTokens []string `env:"APP_DEVICE_TOKENS" envSeparator:","`
	AppAdminToken   string   `env:"APP_ADMIN_TOKEN"`

	// scene description variables
	AppDescriberMode     DescriberMode `env:"APP_DESCRIBER_MODE" envDefault:"llm"`
	AppPlacement         string        `env:"APP_PLACEMENT" envDefault:"dev"`
	AppPrompt            string        `env:"APP_PROMPT" envDefault:"default"`
	AppMaxAnswerChars    int           `env:"APP_MAX_ANSWER_CHARS" envDefault:"220"`
	AppVLMWithDetections bool          `env:"APP_VLM_WITH_DETECTIONS" envDefault:"true"`
	AppSceneFreshnessMS  int           `env:"APP_SCENE_FRESHNESS_MS" envDefault:"10000"`

	// language model variables
	AppLLMURL         string  `env:"APP_LLM_URL" envDefault:"https://polza.ai/api/v1"`
	AppLLMAPIKey      string  `env:"APP_LLM_API_KEY"`
	AppLLMModel       string  `env:"APP_LLM_MODEL"`
	AppVLMModel       string  `env:"APP_VLM_MODEL"`
	AppLLMTemperature float64 `env:"APP_LLM_TEMPERATURE" envDefault:"0"`
	AppLLMSeed        int     `env:"APP_LLM_SEED" envDefault:"42"`
	AppLLMMaxTokens   int     `env:"APP_LLM_MAX_TOKENS" envDefault:"96"`
	AppLLMExtraBody   string  `env:"APP_LLM_EXTRA_BODY" envDefault:"{}"`

	// model services variables
	AppDetectorURL   string `env:"APP_DETECTOR_URL" envDefault:"http://detector:8000"`
	AppDetectorToken string `env:"APP_DETECTOR_TOKEN"`
	AppASRURL        string `env:"APP_ASR_URL" envDefault:"http://asr:8000"`
	AppASRToken      string `env:"APP_ASR_TOKEN"`
	AppTTSURL        string `env:"APP_TTS_URL" envDefault:"http://tts:8000"`
	AppTTSToken      string `env:"APP_TTS_TOKEN"`
	AppTTSVoice      string `env:"APP_TTS_VOICE"`

	// timeout variables, milliseconds
	AppDetectTimeoutMS        int `env:"APP_DETECT_TIMEOUT_MS" envDefault:"400"`
	AppASRTimeoutMS           int `env:"APP_ASR_TIMEOUT_MS" envDefault:"2000"`
	AppLLMTTFTTimeoutMS       int `env:"APP_LLM_TTFT_TIMEOUT_MS" envDefault:"900"`
	AppLLMTotalTimeoutMS      int `env:"APP_LLM_TOTAL_TIMEOUT_MS" envDefault:"5000"`
	AppTTSFirstChunkTimeoutMS int `env:"APP_TTS_FIRST_CHUNK_TIMEOUT_MS" envDefault:"1000"`
	AppRequestDeadlineMS      int `env:"APP_REQUEST_DEADLINE_MS" envDefault:"8000"`
	AppPassiveDeadlineMS      int `env:"APP_PASSIVE_DEADLINE_MS" envDefault:"1500"`

	// passive mode, hazards and intents variables
	AppPassiveNotices   bool   `env:"APP_PASSIVE_NOTICES" envDefault:"false"`
	AppHazardRulesPath  string `env:"APP_HAZARD_RULES_PATH"`
	AppHazardCooldownMS int    `env:"APP_HAZARD_COOLDOWN_MS" envDefault:"10000"`
	AppIntentsPath      string `env:"APP_INTENTS_PATH"`

	// history variables
	AppHistoryScenes int `env:"APP_HISTORY_SCENES" envDefault:"5"`
	AppHistoryTurns  int `env:"APP_HISTORY_TURNS" envDefault:"6"`
	AppHistoryTTLS   int `env:"APP_HISTORY_TTL_S" envDefault:"600"`

	// application variables sent in welcome
	AppImageMaxSide       int `env:"APP_IMAGE_MAX_SIDE" envDefault:"640"`
	AppJPEGQuality        int `env:"APP_JPEG_QUALITY" envDefault:"80"`
	AppPassiveIntervalMS  int `env:"APP_PASSIVE_INTERVAL_MS" envDefault:"3000"`
	AppListenWindowMS     int `env:"APP_LISTEN_WINDOW_MS" envDefault:"5000"`
	AppMaxQuestionMS      int `env:"APP_MAX_QUESTION_MS" envDefault:"15000"`
	AppAudioOutSampleRate int `env:"APP_AUDIO_OUT_SAMPLE_RATE" envDefault:"24000"`
	AppMaxImageBytes      int `env:"APP_MAX_IMAGE_BYTES" envDefault:"2097152"`

	// journal and configuration snapshot variables
	AppPostgresDSN    string `env:"APP_POSTGRES_DSN"`
	AppJournalBuffer  int    `env:"APP_JOURNAL_BUFFER" envDefault:"1024"`
	AppConfigRefreshS int    `env:"APP_CONFIG_REFRESH_S" envDefault:"60"`

	// LogLevel is APP_LOG_LEVEL parsed by Load.
	LogLevel logger.Level
}

// Load reads configuration from environment variables and checks it (PLT-17).
//
// Startup fails on an unknown describer mode or log level, on a missing model
// identifier or a non-http(s) APP_LLM_URL in the llm and vlm modes and, unless
// APP_AUTH_DISABLED=true, on missing device or admin tokens. An empty
// APP_LLM_API_KEY in the llm and vlm modes is only a warning.
func Load() (*Vars, error) {
	vars := &Vars{}
	if err := cenv.Parse(vars); err != nil {
		return nil, fmt.Errorf(errParseFormat, err)
	}

	level, err := logger.ParseLevel(vars.AppLogLevel)
	if err != nil {
		return nil, fmt.Errorf(errLogLevelFormat, err)
	}
	vars.LogLevel = level

	if err := checkDescriber(vars); err != nil {
		return nil, err
	}

	vars.AppDeviceTokens = splitTokens(vars.AppDeviceTokens)
	if !vars.AppAuthDisabled {
		if len(vars.AppDeviceTokens) == 0 {
			return nil, fmt.Errorf(errRequiredFormat, "APP_DEVICE_TOKENS")
		}
		if vars.AppAdminToken == "" {
			return nil, fmt.Errorf(errRequiredFormat, "APP_ADMIN_TOKEN")
		}
	}

	return vars, nil
}

// checkDescriber checks the describer mode and, in the llm and vlm modes, the model and the provider URL.
func checkDescriber(vars *Vars) error {
	switch vars.AppDescriberMode {
	case DescriberModeTemplate:
		return nil
	case DescriberModeLLM:
		if vars.AppLLMModel == "" {
			return errors.New(errLLMModelRequired)
		}
	case DescriberModeVLM:
		if vars.AppVLMModel == "" && vars.AppLLMModel == "" {
			return errors.New(errVLMModelRequired)
		}
	default:
		return fmt.Errorf(errUnknownDescriberMode, string(vars.AppDescriberMode))
	}

	llmURL, err := url.Parse(vars.AppLLMURL)
	if err != nil {
		llmURL = &url.URL{}
	}
	if (llmURL.Scheme != "http" && llmURL.Scheme != "https") || llmURL.Host == "" {
		return fmt.Errorf(errLLMURLScheme, llmURL.Scheme, llmURL.Host)
	}

	if vars.AppLLMAPIKey == "" {
		logger.Warning(warnEmptyLLMAPIKeyFormat, vars.AppDescriberMode)
	}

	return nil
}

// splitTokens trims the tokens and drops the empty ones.
func splitTokens(tokens []string) []string {
	result := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			result = append(result, token)
		}
	}
	return result
}
