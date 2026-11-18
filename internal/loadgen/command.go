package loadgen

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
)

// Exit codes of the command.
const (
	ExitOK = 0
	// ExitFailed - the run could not be made, or with --strict a request failed or answered without audio or timings.
	ExitFailed = 1
	// ExitUsage - invalid flags.
	ExitUsage = 2
)

// Defaults of the flags.
const (
	defaultURL       = "ws://127.0.0.1:8080/v1/session"
	defaultDataset   = "testdata/minidataset"
	defaultScenarios = "describe,ask,passive"
	defaultSessions  = 1
	defaultRuns      = 1
	defaultFirstRun  = 1
	defaultWarmup    = 5 // EXP-3
	defaultTimeoutMS = 30000
	defaultMode      = string(env.DescriberModeLLM)
	tokenEnv         = "LOADGEN_TOKEN"
)

// Texts of the errors and the diagnostics, shared by the code and the tests.
const (
	errArgumentsFormat   = "unexpected arguments: %s"
	errURLFormat         = "--url must be a ws:// or wss:// address: %q"
	errOutRequired       = "--out is required: the JSONL file of the requests"
	errScenarioFormat    = "--scenarios: unknown scenario %q: allowed values are describe, ask, passive"
	errScenariosEmpty    = "--scenarios must name at least one scenario"
	errPositiveFormat    = "--%s must be positive: %d"
	errNegativeFormat    = "--%s must not be negative: %d"
	errModeFormat        = "--describer-mode: unknown mode %q: allowed values are template, llm, vlm"
	errNoQuestions       = "the ask scenario needs questions.jsonl in the data set"
	errCreateFormat      = "failed to create %q: %w"
	errWriteJSONLFormat  = "failed to write the JSONL: %w"
	errWriteSummaryForm  = "failed to write the summary: %w"
	diagErrorFormat      = "loadgen: %v\n"
	diagRunFormat        = "loadgen: run %d: %d requests, %d errors\n"
	diagStrictFormat     = "loadgen: strict: %d requests failed or answered without audio or timings\n"
	diagCancelled        = "loadgen: cancelled\n"
	jsonlFileMode        = 0o644
	scenariosSeparator   = ","
	schemeWS, schemeWSS  = "ws", "wss"
	flagSessions         = "sessions"
	flagPauseMS          = "pause-ms"
	flagRuns             = "runs"
	flagFirstRun         = "first-run"
	flagWarmup           = "warmup"
	flagTimeoutMS        = "timeout-ms"
	summaryIndentPrefix  = ""
	summaryIndent        = "  "
	describerModeOptions = "template, llm or vlm"
)

// options are the command line flags.
type options struct {
	url           string
	token         string
	dataset       string
	scenarios     []string
	sessions      int
	pauseMS       int
	runs          int
	firstRun      int
	warmup        int
	timeoutMS     int
	describerMode string
	out           string
	summary       string
	strict        bool
}

// Main runs the command: the JSONL of the requests goes to --out, the summary to --summary
// or stdout, the diagnostics to stderr. It returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitUsage
	}

	dataset, err := LoadDataset(opts.dataset)
	if err == nil && slices.Contains(opts.scenarios, ScenarioAsk) && len(dataset.Questions) == 0 {
		err = errors.New(errNoQuestions)
	}
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}

	jsonl, err := os.OpenFile(opts.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, jsonlFileMode)
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, fmt.Errorf(errCreateFormat, opts.out, err))
		return ExitFailed
	}
	defer jsonl.Close()

	runner := NewRunner(Config{
		URL:       opts.url,
		Token:     opts.token,
		Scenarios: opts.scenarios,
		Sessions:  opts.sessions,
		Pause:     time.Duration(opts.pauseMS) * time.Millisecond,
		Runs:      opts.runs,
		FirstRun:  opts.firstRun,
		Warmup:    opts.warmup,
		Timeout:   time.Duration(opts.timeoutMS) * time.Millisecond,
	}, dataset)

	if err := runner.Warmup(ctx); err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}

	writer := bufio.NewWriter(jsonl)
	results := []RunResult{}
	for number := opts.firstRun; number < opts.firstRun+opts.runs && ctx.Err() == nil; number++ {
		result := runner.Run(ctx, number)
		results = append(results, result)
		if err := writeJSONL(writer, result.Records); err != nil {
			fmt.Fprintf(stderr, diagErrorFormat, err)
			return ExitFailed
		}
		fmt.Fprintf(stderr, diagRunFormat, number, len(result.Records), countErrors(result.Records))
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, fmt.Errorf(errWriteJSONLFormat, err))
		return ExitFailed
	}

	summary := Summarize(SummaryConfig{
		Sessions:      opts.sessions,
		PauseMS:       opts.pauseMS,
		DescriberMode: opts.describerMode,
		ConfigIDs:     runner.ConfigIDs(),
	}, results)
	if err := writeSummary(summary, opts.summary, stdout); err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}

	if ctx.Err() != nil {
		fmt.Fprint(stderr, diagCancelled)
		return ExitFailed
	}
	if opts.strict {
		if failed := StrictFailures(results); failed > 0 {
			fmt.Fprintf(stderr, diagStrictFormat, failed)
			return ExitFailed
		}
	}
	return ExitOK
}

// StrictFailures counts the requests --strict rejects (DEP-8): every error and every answer
// with text but without audio or without timings. Commands answer without text and audio
// (PROTO-7), passive.clear is not an answer.
func StrictFailures(results []RunResult) int {
	failed := 0
	for _, result := range results {
		for i := range result.Records {
			record := &result.Records[i]
			switch {
			case record.Status == StatusError:
				failed++
			case record.Answered() && record.Text != "" && (record.AudioBytes == 0 || len(record.Timings) == 0):
				failed++
			}
		}
	}
	return failed
}

// parseFlags reads and checks the flags.
func parseFlags(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}
	var scenarios string
	flags := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.url, "url", defaultURL, "address of the gateway session endpoint, ws:// or wss://")
	flags.StringVar(&opts.token, "token", os.Getenv(tokenEnv), "device token (default $"+tokenEnv+"); empty - no Authorization")
	flags.StringVar(&opts.dataset, "dataset", defaultDataset, "directory of the data set (EXP-1)")
	flags.StringVar(&scenarios, "scenarios", defaultScenarios, "scenarios of a run, comma-separated")
	flags.IntVar(&opts.sessions, flagSessions, defaultSessions, "simultaneous sessions")
	flags.IntVar(&opts.pauseMS, flagPauseMS, 0, "pause between the requests of a session, ms")
	flags.IntVar(&opts.runs, flagRuns, defaultRuns, "runs: full passes of the manifest")
	flags.IntVar(&opts.firstRun, flagFirstRun, defaultFirstRun, "number of the first run")
	flags.IntVar(&opts.warmup, flagWarmup, defaultWarmup, "warmup describe requests before the first run, not recorded")
	flags.IntVar(&opts.timeoutMS, flagTimeoutMS, defaultTimeoutMS, "limit of a request from its input to its final message, ms")
	flags.StringVar(&opts.describerMode, "describer-mode", defaultMode, "APP_DESCRIBER_MODE of the gateway: "+describerModeOptions+"; llm_fallback_share counts llm and vlm only")
	flags.StringVar(&opts.out, "out", "", "JSONL file of the requests (required)")
	flags.StringVar(&opts.summary, "summary", "", "summary file; empty - stdout")
	flags.BoolVar(&opts.strict, "strict", false, "exit 1 if a request failed or an answer has no audio or timings")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	if flags.NArg() > 0 {
		return nil, fmt.Errorf(errArgumentsFormat, strings.Join(flags.Args(), " "))
	}
	if parsed, err := url.Parse(opts.url); err != nil || (parsed.Scheme != schemeWS && parsed.Scheme != schemeWSS) || parsed.Host == "" {
		return nil, fmt.Errorf(errURLFormat, opts.url)
	}
	if opts.out == "" {
		return nil, errors.New(errOutRequired)
	}
	var err error
	if opts.scenarios, err = parseScenarios(scenarios); err != nil {
		return nil, err
	}
	switch env.DescriberMode(opts.describerMode) {
	case env.DescriberModeTemplate, env.DescriberModeLLM, env.DescriberModeVLM:
	default:
		return nil, fmt.Errorf(errModeFormat, opts.describerMode)
	}

	for name, value := range map[string]int{flagSessions: opts.sessions, flagRuns: opts.runs, flagFirstRun: opts.firstRun, flagTimeoutMS: opts.timeoutMS} {
		if value < 1 {
			return nil, fmt.Errorf(errPositiveFormat, name, value)
		}
	}
	for name, value := range map[string]int{flagPauseMS: opts.pauseMS, flagWarmup: opts.warmup} {
		if value < 0 {
			return nil, fmt.Errorf(errNegativeFormat, name, value)
		}
	}
	return opts, nil
}

// parseScenarios reads the comma-separated scenarios.
func parseScenarios(value string) ([]string, error) {
	scenarios := []string{}
	for _, name := range strings.Split(value, scenariosSeparator) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(Scenarios, name) {
			return nil, fmt.Errorf(errScenarioFormat, name)
		}
		if !slices.Contains(scenarios, name) {
			scenarios = append(scenarios, name)
		}
	}
	if len(scenarios) == 0 {
		return nil, errors.New(errScenariosEmpty)
	}
	return scenarios, nil
}

// writeJSONL writes a line per record.
func writeJSONL(w io.Writer, records []Record) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			return fmt.Errorf(errWriteJSONLFormat, err)
		}
	}
	return nil
}

// writeSummary writes the summary as indented JSON to the file or to stdout.
func writeSummary(summary *Summary, path string, stdout io.Writer) error {
	body, err := json.MarshalIndent(summary, summaryIndentPrefix, summaryIndent)
	if err != nil {
		return fmt.Errorf(errWriteSummaryForm, err)
	}
	body = append(body, '\n')

	if path == "" {
		if _, err := stdout.Write(body); err != nil {
			return fmt.Errorf(errWriteSummaryForm, err)
		}
		return nil
	}
	if err := os.WriteFile(path, body, jsonlFileMode); err != nil {
		return fmt.Errorf(errCreateFormat, path, err)
	}
	return nil
}

// countErrors counts the failed requests.
func countErrors(records []Record) int {
	failed := 0
	for i := range records {
		if records[i].Status == StatusError {
			failed++
		}
	}
	return failed
}
