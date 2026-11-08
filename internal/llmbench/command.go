package llmbench

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
	"runtime"
	"runtime/debug"
	"strings"

	cenv "github.com/caarlos0/env/v6"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
)

// Exit codes of the command.
const (
	ExitOK = 0
	// ExitFailed - a request failed or answered empty, or the run could not be made.
	ExitFailed = 1
	// ExitUsage - invalid flags.
	ExitUsage = 2
)

// Defaults of the flags: the minimum of LLM-4.
const (
	defaultN      = 50
	defaultWarmup = 5
)

// Build and machine details.
const (
	cpuInfoPath    = "/proc/cpuinfo"
	cpuModelKey    = "model name"
	vcsRevisionKey = "vcs.revision"
	shortSHALength = 7
	unknownValue   = "unknown"
)

// Texts of the errors, shared by the code and the tests.
const (
	errParseEnvFormat   = "failed to parse environment variables: %w"
	errNFormat          = "--n must be positive: %d"
	errWarmupFormat     = "--warmup must not be negative: %d"
	errModelRequired    = "model is required: --model or APP_LLM_MODEL (APP_VLM_MODEL with --image)"
	errImageReadFormat  = "failed to read image %q: %w"
	errImageFormat      = "image %q is not a JPEG file"
	errArgumentsFormat  = "unexpected arguments: %s"
	errWriteFormat      = "failed to write the report: %w"
	diagErrorFormat     = "llmbench: %v\n"
	diagFailedFormat    = "llmbench: %d of %d requests failed or answered empty\n"
	errOutCreateFormat  = "failed to create %q: %w"
	errClientFormat     = "failed to create language model client: %w"
	errEncodeReportForm = "failed to encode the report: %w"
)

// options are the command line flags.
type options struct {
	url    string
	model  string
	n      int
	warmup int
	image  string
	out    string
}

// Main runs the command: the provider settings are APP_LLM_* as in the gateway,
// --url and --model override them. The JSON report goes to stdout or to --out;
// diagnostics go to stderr. It returns the exit code: not 0 if any request,
// the warmup included, failed or answered empty.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, DefaultPromptPath)
}

// run is Main with the path of the reference prompt.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, promptPath string) int {
	opts, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitUsage
	}

	bench, err := newBench(opts, promptPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}

	report, err := bench.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}
	if err := writeReport(report, opts.out, stdout); err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}

	if report.Failed() {
		failed := 0
		for _, count := range report.Errors {
			failed += count
		}
		fmt.Fprintf(stderr, diagFailedFormat, failed, report.Warmup+report.N)
		return ExitFailed
	}
	return ExitOK
}

// parseFlags reads and checks the flags.
func parseFlags(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}
	flags := flag.NewFlagSet("llmbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.url, "url", "", "provider URL; overrides APP_LLM_URL")
	flags.StringVar(&opts.model, "model", "", "model; overrides APP_LLM_MODEL (APP_VLM_MODEL with --image)")
	flags.IntVar(&opts.n, "n", defaultN, "measured requests")
	flags.IntVar(&opts.warmup, "warmup", defaultWarmup, "warmup requests, not measured")
	flags.StringVar(&opts.image, "image", "", "JPEG snapshot attached as in the vlm mode")
	flags.StringVar(&opts.out, "out", "", "report file; empty - stdout")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	switch {
	case flags.NArg() > 0:
		return nil, fmt.Errorf(errArgumentsFormat, strings.Join(flags.Args(), " "))
	case opts.n < 1:
		return nil, fmt.Errorf(errNFormat, opts.n)
	case opts.warmup < 0:
		return nil, fmt.Errorf(errWarmupFormat, opts.warmup)
	}
	return opts, nil
}

// newBench takes the provider settings, the prompt and the image and creates the client of the gateway.
func newBench(opts *options, promptPath string, stderr io.Writer) (*Bench, error) {
	vars := &env.Vars{}
	if err := cenv.Parse(vars); err != nil {
		return nil, fmt.Errorf(errParseEnvFormat, err)
	}
	if opts.url != "" {
		vars.AppLLMURL = opts.url
	}

	model := vars.AppLLMModel
	if opts.image != "" && vars.AppVLMModel != "" {
		model = vars.AppVLMModel
	}
	if opts.model != "" {
		model = opts.model
	}
	if model == "" {
		return nil, errors.New(errModelRequired)
	}

	prompt, err := LoadPromptFile(promptPath)
	if err != nil {
		return nil, err
	}
	var image []byte
	if opts.image != "" {
		if image, err = readJPEG(opts.image); err != nil {
			return nil, err
		}
	}

	client, err := llm.NewClient(llm.NewConfig(vars), metrics.NewMetrics())
	if err != nil {
		return nil, fmt.Errorf(errClientFormat, err)
	}

	backend := ""
	if parsed, err := url.Parse(vars.AppLLMURL); err == nil {
		backend = parsed.Hostname()
	}
	cfg := Config{
		Backend:      backend,
		Model:        model,
		N:            opts.n,
		Warmup:       opts.warmup,
		Messages:     prompt.Request(image),
		GitSHA:       gitSHA(),
		Hardware:     Hardware{CPU: cpuModel()},
		MeasuredFrom: hostname(),
	}
	return NewBench(client, cfg, stderr), nil
}

// readJPEG reads the snapshot of --image and checks the JPEG SOI marker.
func readJPEG(path string) ([]byte, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errImageReadFormat, path, err)
	}
	if len(image) < 3 || image[0] != 0xFF || image[1] != 0xD8 || image[2] != 0xFF {
		return nil, fmt.Errorf(errImageFormat, path)
	}
	return image, nil
}

// writeReport writes the report as one line of JSON.
func writeReport(report *Report, out string, stdout io.Writer) error {
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf(errEncodeReportForm, err)
	}
	body = append(body, '\n')

	if out == "" {
		if _, err := stdout.Write(body); err != nil {
			return fmt.Errorf(errWriteFormat, err)
		}
		return nil
	}
	if err := os.WriteFile(out, body, 0o644); err != nil {
		return fmt.Errorf(errOutCreateFormat, out, err)
	}
	return nil
}

// gitSHA is the short commit of the build, "unknown" for a build without VCS data.
func gitSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownValue
	}
	for _, setting := range info.Settings {
		if setting.Key == vcsRevisionKey && setting.Value != "" {
			return setting.Value[:min(len(setting.Value), shortSHALength)]
		}
	}
	return unknownValue
}

// cpuModel is the CPU model of /proc/cpuinfo, the architecture if it is not readable.
func cpuModel() string {
	file, err := os.Open(cpuInfoPath)
	if err != nil {
		return runtime.GOARCH
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(key) == cpuModelKey {
			return strings.TrimSpace(value)
		}
	}
	return runtime.GOARCH
}

// hostname is the name of the measuring machine.
func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return unknownValue
	}
	return name
}
