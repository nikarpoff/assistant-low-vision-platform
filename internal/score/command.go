package score

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Exit codes of the command.
const (
	ExitOK = 0
	// ExitFailed - an input could not be read or is inconsistent.
	ExitFailed = 1
	// ExitUsage - invalid flags.
	ExitUsage = 2
)

// Defaults of the flags.
const (
	defaultDataset    = "testdata/minidataset"
	defaultVocabulary = "testdata/contracts/objects-detector/vocabulary.response.json"
)

// Texts of the command errors and diagnostics, shared by the code and the tests.
const (
	errNoFiles         = "at least one JSONL file of loadgen is required"
	errWriteFormat     = "failed to write the result: %w"
	diagErrorFormat    = "score: %v\n"
	resultFileMode     = 0o644
	resultIndentPrefix = ""
	resultIndent       = "  "
)

// options are the command line flags.
type options struct {
	dataset    string
	vocabulary string
	stats      string
	out        string
	files      []string
}

// Main runs the command: the JSON result goes to --out or stdout, the diagnostics to stderr.
// It returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitUsage
	}

	result, err := run(opts)
	if err == nil {
		err = writeResult(result, opts.out, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, diagErrorFormat, err)
		return ExitFailed
	}
	return ExitOK
}

// run reads the inputs, scores the answers and with --stats gives the verdict.
func run(opts *options) (*Result, error) {
	vocab, err := loadVocabulary(opts.vocabulary)
	if err != nil {
		return nil, err
	}
	images, err := LoadDataset(opts.dataset, vocab)
	if err != nil {
		return nil, err
	}
	input, err := ReadRecords(opts.files)
	if err != nil {
		return nil, err
	}
	result, err := NewScorer(vocab, images).Score(input)
	if err != nil {
		return nil, err
	}

	if opts.stats != "" {
		response, err := loadStats(opts.stats)
		if err != nil {
			return nil, err
		}
		if result.Hypothesis, err = BuildHypothesis(result, response); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// parseFlags reads and checks the flags; the arguments are the JSONL files.
func parseFlags(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}
	flags := flag.NewFlagSet("score", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.dataset, "dataset", defaultDataset, "directory of the data set (EXP-1): descriptions.jsonl and annotations.json")
	flags.StringVar(&opts.vocabulary, "vocabulary", defaultVocabulary, "the detector vocabulary: a saved response of GET /v1/vocabulary")
	flags.StringVar(&opts.stats, "stats", "", "a saved response of GET /v1/stats for the config_id over the runs: adds the hypothesis verdict (EXP-8)")
	flags.StringVar(&opts.out, "out", "", "result file; empty - stdout")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: score [flags] <loadgen JSONL> [<loadgen JSONL>...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	opts.files = flags.Args()
	if len(opts.files) == 0 {
		return nil, errors.New(errNoFiles)
	}
	return opts, nil
}

// loadVocabulary reads a saved response of GET /v1/vocabulary.
func loadVocabulary(path string) (*vocabulary.Vocabulary, error) {
	var document vocabulary.Document
	if err := readJSON(path, &document); err != nil {
		return nil, err
	}
	vocab, err := vocabulary.NewVocabulary(&document)
	if err != nil {
		return nil, fmt.Errorf(errParseFileFormat, path, err)
	}
	return vocab, nil
}

// loadStats reads a saved response of GET /v1/stats.
func loadStats(path string) (*stats.GetStatsResponse, error) {
	var response stats.GetStatsResponse
	if err := readJSON(path, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// readJSON reads a JSON file into value.
func readJSON(path string, value any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf(errReadFileFormat, path, err)
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf(errParseFileFormat, path, err)
	}
	return nil
}

// writeResult writes the result as indented JSON to the file or to stdout.
func writeResult(result *Result, path string, stdout io.Writer) error {
	body, err := json.MarshalIndent(result, resultIndentPrefix, resultIndent)
	if err != nil {
		return fmt.Errorf(errWriteFormat, err)
	}
	body = append(body, '\n')

	if path == "" {
		if _, err := stdout.Write(body); err != nil {
			return fmt.Errorf(errWriteFormat, err)
		}
		return nil
	}
	if err := os.WriteFile(path, body, resultFileMode); err != nil {
		return fmt.Errorf(errWriteFormat, err)
	}
	return nil
}
