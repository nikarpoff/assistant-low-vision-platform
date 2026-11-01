package fakellm

import (
	"errors"
	"flag"
	"fmt"
	"slices"

	cenv "github.com/caarlos0/env/v6"
)

// FailMode is the failure the stub imitates (FAKELLM_FAIL, PLT-19).
type FailMode string

// Failure modes of the stub.
const (
	// FailNone streams the answer normally.
	FailNone FailMode = "none"
	// FailHTTP402 answers 402 INSUFFICIENT_BALANCE before the stream.
	FailHTTP402 FailMode = "http_402"
	// FailHTTP429 answers 429 TOO_MANY_REQUESTS with Retry-After: 1 before the stream.
	FailHTTP429 FailMode = "http_429"
	// FailHTTP500 answers 500 INTERNAL_ERROR before the stream.
	FailHTTP500 FailMode = "http_500"
	// FailStreamErrorBeforeText sends status 200 and an error event SERVICE_UNAVAILABLE as the first chunk, then [DONE].
	FailStreamErrorBeforeText FailMode = "stream_error_before_text"
	// FailStreamErrorAfterSentence sends the first sentence, then an error event SERVICE_UNAVAILABLE and [DONE].
	FailStreamErrorAfterSentence FailMode = "stream_error_after_sentence"
	// FailStream402AfterSentence sends the first sentence, then an error event INSUFFICIENT_BALANCE and [DONE].
	FailStream402AfterSentence FailMode = "stream_402_after_sentence"
	// FailDisconnectAfterSentence sends the first sentence and closes the connection without finish_reason and [DONE].
	FailDisconnectAfterSentence FailMode = "disconnect_after_sentence"
	// FailEmpty sends status 200, a chunk with finish_reason "stop" and [DONE] without any text.
	FailEmpty FailMode = "empty"
)

// FailModes lists every value of FAKELLM_FAIL.
var FailModes = []FailMode{
	FailNone,
	FailHTTP402,
	FailHTTP429,
	FailHTTP500,
	FailStreamErrorBeforeText,
	FailStreamErrorAfterSentence,
	FailStream402AfterSentence,
	FailDisconnectAfterSentence,
	FailEmpty,
}

// Texts of the configuration errors, shared by the code and the tests.
const (
	errParseEnvFormat    = "failed to parse environment variables: %w"
	errParseFlagsFormat  = "failed to parse flags: %w"
	errUnknownFailFormat = "unknown FAKELLM_FAIL %q: allowed values are none, http_402, http_429, http_500, stream_error_before_text, stream_error_after_sentence, stream_402_after_sentence, disconnect_after_sentence, empty"
	errPortFormat        = "FAKELLM_PORT must be between 1 and 65535: %d"
	errDelayFormat       = "%s must not be negative: %d"
	errModelRequired     = "FAKELLM_MODEL must not be empty"
)

// flagAPIKey is the flag of the key; unlike the other flags its default is not taken from the environment.
const flagAPIKey = "api-key"

// Config holds the settings of the stub. The command reads them from FAKELLM_*
// variables and flags (flags win); tests in process fill the struct directly.
type Config struct {
	Port              int      `env:"FAKELLM_PORT" envDefault:"8000"`
	Model             string   `env:"FAKELLM_MODEL" envDefault:"fake/fake-llm"`
	APIKey            string   `env:"FAKELLM_API_KEY"`
	FirstTokenDelayMS int      `env:"FAKELLM_FIRST_TOKEN_DELAY_MS" envDefault:"0"`
	TokenDelayMS      int      `env:"FAKELLM_TOKEN_DELAY_MS" envDefault:"0"`
	Fail              FailMode `env:"FAKELLM_FAIL" envDefault:"none"`

	// Text is the answer; empty means DefaultText. Only tests in process set it.
	Text string
}

// LoadConfig reads FAKELLM_* variables, then the command line flags, and checks the result.
// For -h it returns flag.ErrHelp.
func LoadConfig(args []string) (*Config, error) {
	cfg := &Config{}
	if err := cenv.Parse(cfg); err != nil {
		return nil, fmt.Errorf(errParseEnvFormat, err)
	}

	// The key from the environment is not a flag default: -h would print it
	fail := string(cfg.Fail)
	apiKey := ""
	flags := flag.NewFlagSet("fakellm", flag.ContinueOnError)
	flags.IntVar(&cfg.Port, "port", cfg.Port, "HTTP port (FAKELLM_PORT)")
	flags.StringVar(&cfg.Model, "model", cfg.Model, "model in the chunks and in /v1/models (FAKELLM_MODEL)")
	flags.StringVar(&apiKey, flagAPIKey, "", "required Bearer key; empty - no authentication (FAKELLM_API_KEY)")
	flags.IntVar(&cfg.FirstTokenDelayMS, "first-token-delay-ms", cfg.FirstTokenDelayMS, "delay of the first token, ms (FAKELLM_FIRST_TOKEN_DELAY_MS)")
	flags.IntVar(&cfg.TokenDelayMS, "token-delay-ms", cfg.TokenDelayMS, "delay between tokens, ms (FAKELLM_TOKEN_DELAY_MS)")
	flags.StringVar(&fail, "fail", fail, "failure mode (FAKELLM_FAIL)")
	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf(errParseFlagsFormat, err)
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == flagAPIKey {
			cfg.APIKey = apiKey
		}
	})
	cfg.Fail = FailMode(fail)

	if err := cfg.check(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// check validates the settings read by LoadConfig.
func (c *Config) check() error {
	if !slices.Contains(FailModes, c.Fail) {
		return fmt.Errorf(errUnknownFailFormat, string(c.Fail))
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf(errPortFormat, c.Port)
	}
	if c.FirstTokenDelayMS < 0 {
		return fmt.Errorf(errDelayFormat, "FAKELLM_FIRST_TOKEN_DELAY_MS", c.FirstTokenDelayMS)
	}
	if c.TokenDelayMS < 0 {
		return fmt.Errorf(errDelayFormat, "FAKELLM_TOKEN_DELAY_MS", c.TokenDelayMS)
	}
	if c.Model == "" {
		return errors.New(errModelRequired)
	}
	return nil
}
