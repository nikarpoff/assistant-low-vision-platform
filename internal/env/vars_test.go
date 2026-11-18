package env

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

func TestLoad(t *testing.T) {
	// variables without defaults that the llm mode with authentication needs
	secrets := map[string]string{
		"APP_DEVICE_TOKENS": "device-token",
		"APP_ADMIN_TOKEN":   "admin-token",
		"APP_LLM_MODEL":     "qwen/qwen3-8b",
	}

	var getLogs func() string

	// setup removes every APP_* variable of the process for the subtest and sets the given ones.
	setup := func(t *testing.T, values map[string]string) {
		getLogs = logger.InitTestLogger()
		for _, entry := range os.Environ() {
			key, value, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(key, "APP_") {
				t.Setenv(key, value)
				require.NoError(t, os.Unsetenv(key))
			}
		}
		for key, value := range values {
			t.Setenv(key, value)
		}
	}

	// with returns the secrets with the given variables added or replaced.
	with := func(values map[string]string) map[string]string {
		result := map[string]string{}
		for key, value := range secrets {
			result[key] = value
		}
		for key, value := range values {
			result[key] = value
		}
		return result
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(t, secrets)

		vars, err := Load()

		require.NoError(t, err)
		require.Equal(t, &Vars{
			AppServerPort:            80,
			AppServerReadTimeout:     15,
			AppServerWriteTimeout:    15,
			AppServerIdleTimeout:     60,
			AppServerShutdownTimeout: 5,
			AppLogLevel:              "INFO",

			AppAuthDisabled: false,
			AppDeviceTokens: []string{"device-token"},
			AppAdminToken:   "admin-token",

			AppDescriberMode:     DescriberModeLLM,
			AppPlacement:         "dev",
			AppPrompt:            "default",
			AppMaxAnswerChars:    220,
			AppVLMWithDetections: true,
			AppSceneFreshnessMS:  10000,

			AppLLMURL:         "https://polza.ai/api/v1",
			AppLLMAPIKey:      "",
			AppLLMModel:       "qwen/qwen3-8b",
			AppVLMModel:       "",
			AppLLMTemperature: 0,
			AppLLMSeed:        42,
			AppLLMMaxTokens:   96,
			AppLLMExtraBody:   "{}",

			AppDetectorURL:   "http://detector:8000",
			AppDetectorToken: "",
			AppASRURL:        "http://asr:8000",
			AppASRToken:      "",
			AppTTSURL:        "http://tts:8000",
			AppTTSToken:      "",
			AppTTSVoice:      "",

			AppDetectTimeoutMS:        400,
			AppASRTimeoutMS:           2000,
			AppLLMTTFTTimeoutMS:       900,
			AppLLMTotalTimeoutMS:      5000,
			AppTTSFirstChunkTimeoutMS: 1000,
			AppRequestDeadlineMS:      8000,
			AppPassiveDeadlineMS:      1500,

			AppPassiveNotices:   false,
			AppHazardRulesPath:  "",
			AppHazardCooldownMS: 10000,
			AppIntentsPath:      "",

			AppHistoryScenes: 5,
			AppHistoryTurns:  6,
			AppHistoryTTLS:   600,

			AppImageMaxSide:       640,
			AppJPEGQuality:        80,
			AppPassiveIntervalMS:  3000,
			AppListenWindowMS:     5000,
			AppMaxQuestionMS:      15000,
			AppAudioOutSampleRate: 24000,
			AppMaxImageBytes:      2097152,

			AppPostgresDSN:    "",
			AppJournalBuffer:  1024,
			AppConfigRefreshS: 60,

			LogLevel: logger.LevelInfo,
		}, vars)
	})

	t.Run("must_pass_overrides", func(t *testing.T) {
		setup(t, map[string]string{
			"APP_SERVER_PORT":                "8080",
			"APP_SERVER_READ_TIMEOUT":        "20",
			"APP_SERVER_WRITE_TIMEOUT":       "25",
			"APP_SERVER_IDLE_TIMEOUT":        "90",
			"APP_SERVER_SHUTDOWN_TIMEOUT":    "7",
			"APP_LOG_LEVEL":                  "debug",
			"APP_AUTH_DISABLED":              "false",
			"APP_DEVICE_TOKENS":              " phone-1 ,phone-2,, ",
			"APP_ADMIN_TOKEN":                "admin",
			"APP_DESCRIBER_MODE":             "vlm",
			"APP_PLACEMENT":                  "B",
			"APP_PROMPT":                     "short",
			"APP_MAX_ANSWER_CHARS":           "180",
			"APP_VLM_WITH_DETECTIONS":        "false",
			"APP_SCENE_FRESHNESS_MS":         "7000",
			"APP_LLM_URL":                    "http://gpt2giga:8090/v1",
			"APP_LLM_API_KEY":                "provider-key",
			"APP_LLM_MODEL":                  "GigaChat-2",
			"APP_VLM_MODEL":                  "GigaChat-2-Max",
			"APP_LLM_TEMPERATURE":            "0.3",
			"APP_LLM_SEED":                   "7",
			"APP_LLM_MAX_TOKENS":             "128",
			"APP_LLM_EXTRA_BODY":             `{"reasoning":{"enabled":false}}`,
			"APP_DETECTOR_URL":               "http://10.77.0.2:8101",
			"APP_DETECTOR_TOKEN":             "detector-token",
			"APP_ASR_URL":                    "http://10.77.0.2:8102",
			"APP_ASR_TOKEN":                  "asr-token",
			"APP_TTS_URL":                    "http://10.77.0.2:8103",
			"APP_TTS_TOKEN":                  "tts-token",
			"APP_TTS_VOICE":                  "ruslan",
			"APP_DETECT_TIMEOUT_MS":          "500",
			"APP_ASR_TIMEOUT_MS":             "2500",
			"APP_LLM_TTFT_TIMEOUT_MS":        "1200",
			"APP_LLM_TOTAL_TIMEOUT_MS":       "6000",
			"APP_TTS_FIRST_CHUNK_TIMEOUT_MS": "1100",
			"APP_REQUEST_DEADLINE_MS":        "9000",
			"APP_PASSIVE_DEADLINE_MS":        "1600",
			"APP_PASSIVE_NOTICES":            "true",
			"APP_HAZARD_RULES_PATH":          "/etc/alv/hazard_rules.yaml",
			"APP_HAZARD_COOLDOWN_MS":         "12000",
			"APP_INTENTS_PATH":               "/etc/alv/intents.yaml",
			"APP_HISTORY_SCENES":             "3",
			"APP_HISTORY_TURNS":              "4",
			"APP_HISTORY_TTL_S":              "300",
			"APP_IMAGE_MAX_SIDE":             "800",
			"APP_JPEG_QUALITY":               "75",
			"APP_PASSIVE_INTERVAL_MS":        "2500",
			"APP_LISTEN_WINDOW_MS":           "4000",
			"APP_MAX_QUESTION_MS":            "10000",
			"APP_AUDIO_OUT_SAMPLE_RATE":      "22050",
			"APP_MAX_IMAGE_BYTES":            "1048576",
			"APP_POSTGRES_DSN":               "postgres://alv:secret@postgres:5432/alv",
			"APP_JOURNAL_BUFFER":             "256",
			"APP_CONFIG_REFRESH_S":           "30",
		})

		vars, err := Load()

		require.NoError(t, err)
		require.Equal(t, &Vars{
			AppServerPort:            8080,
			AppServerReadTimeout:     20,
			AppServerWriteTimeout:    25,
			AppServerIdleTimeout:     90,
			AppServerShutdownTimeout: 7,
			AppLogLevel:              "debug",

			AppAuthDisabled: false,
			AppDeviceTokens: []string{"phone-1", "phone-2"},
			AppAdminToken:   "admin",

			AppDescriberMode:     DescriberModeVLM,
			AppPlacement:         "B",
			AppPrompt:            "short",
			AppMaxAnswerChars:    180,
			AppVLMWithDetections: false,
			AppSceneFreshnessMS:  7000,

			AppLLMURL:         "http://gpt2giga:8090/v1",
			AppLLMAPIKey:      "provider-key",
			AppLLMModel:       "GigaChat-2",
			AppVLMModel:       "GigaChat-2-Max",
			AppLLMTemperature: 0.3,
			AppLLMSeed:        7,
			AppLLMMaxTokens:   128,
			AppLLMExtraBody:   `{"reasoning":{"enabled":false}}`,

			AppDetectorURL:   "http://10.77.0.2:8101",
			AppDetectorToken: "detector-token",
			AppASRURL:        "http://10.77.0.2:8102",
			AppASRToken:      "asr-token",
			AppTTSURL:        "http://10.77.0.2:8103",
			AppTTSToken:      "tts-token",
			AppTTSVoice:      "ruslan",

			AppDetectTimeoutMS:        500,
			AppASRTimeoutMS:           2500,
			AppLLMTTFTTimeoutMS:       1200,
			AppLLMTotalTimeoutMS:      6000,
			AppTTSFirstChunkTimeoutMS: 1100,
			AppRequestDeadlineMS:      9000,
			AppPassiveDeadlineMS:      1600,

			AppPassiveNotices:   true,
			AppHazardRulesPath:  "/etc/alv/hazard_rules.yaml",
			AppHazardCooldownMS: 12000,
			AppIntentsPath:      "/etc/alv/intents.yaml",

			AppHistoryScenes: 3,
			AppHistoryTurns:  4,
			AppHistoryTTLS:   300,

			AppImageMaxSide:       800,
			AppJPEGQuality:        75,
			AppPassiveIntervalMS:  2500,
			AppListenWindowMS:     4000,
			AppMaxQuestionMS:      10000,
			AppAudioOutSampleRate: 22050,
			AppMaxImageBytes:      1048576,

			AppPostgresDSN:    "postgres://alv:secret@postgres:5432/alv",
			AppJournalBuffer:  256,
			AppConfigRefreshS: 30,

			LogLevel: logger.LevelDebug,
		}, vars)
		require.Empty(t, getLogs())
	})

	t.Run("must_pass_auth_disabled_without_secrets", func(t *testing.T) {
		setup(t, map[string]string{"APP_AUTH_DISABLED": "true", "APP_LLM_MODEL": "fake/fake-llm"})

		vars, err := Load()

		require.NoError(t, err)
		require.True(t, vars.AppAuthDisabled)
		require.Empty(t, vars.AppDeviceTokens)
		require.Empty(t, vars.AppAdminToken)
	})

	t.Run("must_pass_template_mode_without_llm", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DESCRIBER_MODE": "template", "APP_LLM_MODEL": "", "APP_LLM_URL": "ftp://example.com"}))

		vars, err := Load()

		require.NoError(t, err)
		require.Equal(t, DescriberModeTemplate, vars.AppDescriberMode)
		require.Empty(t, getLogs())
	})

	t.Run("must_pass_vlm_model_from_llm_model", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DESCRIBER_MODE": "vlm", "APP_LLM_API_KEY": "provider-key"}))

		vars, err := Load()

		require.NoError(t, err)
		require.Equal(t, DescriberModeVLM, vars.AppDescriberMode)
		require.Empty(t, vars.AppVLMModel)
		require.Equal(t, "qwen/qwen3-8b", vars.AppLLMModel)
	})

	t.Run("must_pass_empty_llm_api_key_warning", func(t *testing.T) {
		setup(t, secrets)

		_, err := Load()

		require.NoError(t, err)
		require.Equal(t, "WARNING: "+fmt.Sprintf(warnEmptyLLMAPIKeyFormat, "llm")+"\n", getLogs())
	})

	t.Run("must_fail_unknown_describer_mode", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DESCRIBER_MODE": "smart"}))

		vars, err := Load()

		require.Nil(t, vars)
		require.EqualError(t, err, fmt.Sprintf(errUnknownDescriberMode, "smart"))
		require.EqualError(t, err, `unknown APP_DESCRIBER_MODE "smart": allowed values are template, llm, vlm`)
	})

	t.Run("must_fail_llm_model_empty", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DESCRIBER_MODE": "llm", "APP_LLM_MODEL": "", "APP_VLM_MODEL": "qwen/qwen2.5-vl-7b"}))

		vars, err := Load()

		require.Nil(t, vars)
		require.EqualError(t, err, errLLMModelRequired)
	})

	t.Run("must_fail_vlm_models_empty", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DESCRIBER_MODE": "vlm", "APP_LLM_MODEL": "", "APP_VLM_MODEL": ""}))

		vars, err := Load()

		require.Nil(t, vars)
		require.EqualError(t, err, errVLMModelRequired)
	})

	t.Run("must_fail_llm_url_scheme", func(t *testing.T) {
		for _, tc := range []struct {
			mode   string
			url    string
			scheme string
			host   string
		}{
			{mode: "llm", url: "ftp://polza.ai/api/v1", scheme: "ftp", host: "polza.ai"},
			{mode: "vlm", url: "polza.ai/api/v1", scheme: "", host: ""},
			{mode: "llm", url: "https://", scheme: "https", host: ""},
			{mode: "llm", url: "://polza.ai", scheme: "", host: ""},
		} {
			setup(t, nil)
			setup(t, with(map[string]string{"APP_DESCRIBER_MODE": tc.mode, "APP_LLM_URL": tc.url}))

			vars, err := Load()

			require.Nil(t, vars)
			require.EqualError(t, err, fmt.Sprintf(errLLMURLScheme, tc.scheme, tc.host))
		}
	})

	t.Run("must_fail_device_tokens_required", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_DEVICE_TOKENS": " , "}))

		vars, err := Load()

		require.Nil(t, vars)
		require.EqualError(t, err, fmt.Sprintf(errRequiredFormat, "APP_DEVICE_TOKENS"))
	})

	t.Run("must_fail_admin_token_required", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_ADMIN_TOKEN": ""}))

		vars, err := Load()

		require.Nil(t, vars)
		require.EqualError(t, err, fmt.Sprintf(errRequiredFormat, "APP_ADMIN_TOKEN"))
	})

	t.Run("must_fail_unknown_log_level", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_LOG_LEVEL": "verbose"}))

		vars, err := Load()

		require.Nil(t, vars)
		_, levelErr := logger.ParseLevel("verbose")
		require.EqualError(t, err, fmt.Errorf(errLogLevelFormat, levelErr).Error())
	})

	t.Run("must_fail_invalid_number", func(t *testing.T) {
		setup(t, with(map[string]string{"APP_LLM_TTFT_TIMEOUT_MS": "fast"}))

		vars, err := Load()

		require.Nil(t, vars)
		require.ErrorContains(t, err, "failed to parse environment variables: ")
		require.ErrorContains(t, err, "AppLLMTTFTTimeoutMS")
	})
}
