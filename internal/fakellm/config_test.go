package fakellm

import (
	"flag"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	var (
		args []string
	)

	setup := func(t *testing.T, env map[string]string, flags ...string) {
		for _, name := range []string{"FAKELLM_PORT", "FAKELLM_MODEL", "FAKELLM_API_KEY", "FAKELLM_FIRST_TOKEN_DELAY_MS", "FAKELLM_TOKEN_DELAY_MS", "FAKELLM_FAIL"} {
			if value, ok := env[name]; ok {
				t.Setenv(name, value)
			}
		}
		args = flags
	}

	t.Run("must_pass_defaults", func(t *testing.T) {
		setup(t, nil)

		cfg, err := LoadConfig(args)

		require.NoError(t, err)
		require.Equal(t, &Config{Port: 8000, Model: DefaultModel, Fail: FailNone}, cfg)
	})

	t.Run("must_pass_env", func(t *testing.T) {
		setup(t, map[string]string{
			"FAKELLM_PORT":                 "8090",
			"FAKELLM_MODEL":                "qwen/qwen3-test",
			"FAKELLM_API_KEY":              "test-key",
			"FAKELLM_FIRST_TOKEN_DELAY_MS": "300",
			"FAKELLM_TOKEN_DELAY_MS":       "20",
			"FAKELLM_FAIL":                 "http_429",
		})

		cfg, err := LoadConfig(args)

		require.NoError(t, err)
		require.Equal(t, &Config{Port: 8090, Model: "qwen/qwen3-test", APIKey: "test-key", FirstTokenDelayMS: 300, TokenDelayMS: 20, Fail: FailHTTP429}, cfg)
	})

	t.Run("must_pass_flags_override_env", func(t *testing.T) {
		setup(t, map[string]string{"FAKELLM_PORT": "8090", "FAKELLM_FAIL": "http_429", "FAKELLM_API_KEY": "env-key"},
			"--port", "9000", "--model", "flag/model", "--api-key", "flag-key",
			"--first-token-delay-ms", "50", "--token-delay-ms", "5", "--fail", "empty")

		cfg, err := LoadConfig(args)

		require.NoError(t, err)
		require.Equal(t, &Config{Port: 9000, Model: "flag/model", APIKey: "flag-key", FirstTokenDelayMS: 50, TokenDelayMS: 5, Fail: FailEmpty}, cfg)
	})

	t.Run("must_pass_empty_key_flag_disables_env_key", func(t *testing.T) {
		setup(t, map[string]string{"FAKELLM_API_KEY": "env-key"}, "--api-key=")

		cfg, err := LoadConfig(args)

		require.NoError(t, err)
		require.Empty(t, cfg.APIKey)
	})

	t.Run("must_pass_every_fail_mode", func(t *testing.T) {
		for _, mode := range FailModes {
			setup(t, map[string]string{"FAKELLM_FAIL": string(mode)})

			cfg, err := LoadConfig(args)

			require.NoError(t, err)
			require.Equal(t, mode, cfg.Fail)
		}
	})

	t.Run("must_fail_unknown_fail_mode", func(t *testing.T) {
		setup(t, map[string]string{"FAKELLM_FAIL": "timeout"})

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.EqualError(t, err, fmt.Sprintf(errUnknownFailFormat, "timeout"))
	})

	t.Run("must_fail_port", func(t *testing.T) {
		setup(t, nil, "--port", "0")

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.EqualError(t, err, fmt.Sprintf(errPortFormat, 0))
	})

	t.Run("must_fail_negative_first_token_delay", func(t *testing.T) {
		setup(t, map[string]string{"FAKELLM_FIRST_TOKEN_DELAY_MS": "-1"})

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.EqualError(t, err, fmt.Sprintf(errDelayFormat, "FAKELLM_FIRST_TOKEN_DELAY_MS", -1))
	})

	t.Run("must_fail_negative_token_delay", func(t *testing.T) {
		setup(t, nil, "--token-delay-ms", "-5")

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.EqualError(t, err, fmt.Sprintf(errDelayFormat, "FAKELLM_TOKEN_DELAY_MS", -5))
	})

	t.Run("must_fail_empty_model", func(t *testing.T) {
		setup(t, nil, "--model", "")

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.EqualError(t, err, errModelRequired)
	})

	t.Run("must_fail_invalid_env", func(t *testing.T) {
		setup(t, map[string]string{"FAKELLM_PORT": "port"})

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.ErrorContains(t, err, "failed to parse environment variables: ")
	})

	t.Run("must_fail_help", func(t *testing.T) {
		setup(t, nil, "-h")

		cfg, err := LoadConfig(args)

		require.Nil(t, cfg)
		require.ErrorIs(t, err, flag.ErrHelp)
		require.EqualError(t, err, fmt.Errorf(errParseFlagsFormat, flag.ErrHelp).Error())
	})
}
