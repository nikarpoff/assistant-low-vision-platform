package score

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Toy data of the tests (testdata/score).
const (
	testVocabulary = "../../testdata/contracts/objects-detector/vocabulary.response.json"
	// testMetrics: 3 snapshots with 2 key objects each and two runs in two files.
	testMetrics = "../../testdata/score/metrics"
	// testVerdict: 20 snapshots with one key object each; every quality<N>.jsonl has 5 runs,
	// 100 key objects of which N are named with the class and the direction.
	testVerdict  = "../../testdata/score/verdict"
	testConfigID = "c9f2a1d0e3b4"
	testDelta    = 1e-9
)

func TestScore(t *testing.T) {
	var (
		scorer *Scorer
		images map[int]*Image
	)

	setup := func(t *testing.T, dataset string) {
		t.Helper()
		vocab, err := loadVocabulary(testVocabulary)
		require.NoError(t, err)
		images, err = LoadDataset(dataset, vocab)
		require.NoError(t, err)
		scorer = NewScorer(vocab, images)
	}

	// scoreFiles scores the JSONL files of the data set.
	scoreFiles := func(t *testing.T, dataset string, files ...string) *Result {
		t.Helper()
		paths := []string{}
		for _, file := range files {
			paths = append(paths, filepath.Join(dataset, file))
		}
		input, err := ReadRecords(paths)
		require.NoError(t, err)
		result, err := scorer.Score(input)
		require.NoError(t, err)
		return result
	}

	// runCommand runs cmd/score over the verdict data and decodes its JSON.
	runCommand := func(t *testing.T, jsonl, statsFile string) (*Result, map[string]any) {
		t.Helper()
		args := []string{
			"--dataset", testVerdict,
			"--vocabulary", testVocabulary,
			"--stats", filepath.Join(testVerdict, statsFile),
			filepath.Join(testVerdict, jsonl),
		}
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		require.Equal(t, ExitOK, Main(args, stdout, stderr), stderr.String())

		result := &Result{}
		require.NoError(t, json.Unmarshal(stdout.Bytes(), result))
		raw := map[string]any{}
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &raw))
		require.NotNil(t, result.Hypothesis)
		return result, raw
	}

	requireShare := func(t *testing.T, expected float64, actual *float64) {
		t.Helper()
		require.NotNil(t, actual)
		require.InDelta(t, expected, *actual, testDelta)
	}

	requireChecks := func(t *testing.T, h *Hypothesis, e2e, quality, fallbacks *bool) {
		t.Helper()
		require.Equal(t, e2e, h.Checks.E2E, "e2e")
		require.Equal(t, quality, h.Checks.Quality, "quality")
		require.Equal(t, fallbacks, h.Checks.Fallbacks, "fallbacks")
	}

	yes, no := check(true), check(false)

	t.Run("must_pass_key_objects_with_direction", func(t *testing.T) {
		setup(t, testMetrics)

		tests := []struct {
			image    int
			text     string
			expected int
		}{
			{image: 1, text: "Впереди стол. Справа дверь.", expected: 2},
			{image: 1, text: "Справа дверь, впереди стол.", expected: 2},                // one sentence, both directions
			{image: 1, text: "Дверь и стол. Справа и впереди.", expected: 0},            // direction in another sentence
			{image: 1, text: "Слева дверь. Впереди стол.", expected: 1},                 // wrong direction of the door
			{image: 1, text: "Справа от вас ДВЕРЬ! Прямо - столик…", expected: 2},       // forms, synonyms, punctuation
			{image: 2, text: "Впереди стул, слева ещё стул.", expected: 2},              // two objects of a class
			{image: 2, text: "Впереди стул.", expected: 1},                              // the chair on the left is missed
			{image: 3, text: "Справа машина. Впереди пешеходный переход.", expected: 2}, // multi-word name by its mention
			{image: 3, text: "Справа переход. Впереди машина и человек.", expected: 0},  // classes and directions swapped
			{image: 3, text: "", expected: 0}, // empty answer
		}
		for _, test := range tests {
			totals := scorer.ScoreAnswer(test.text, images[test.image])
			require.Equal(t, test.expected, totals.NamedWithDirection, test.text)
			require.Equal(t, 2, totals.KeyObjects, test.text)
		}

		result := scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl")
		require.Equal(t, 10, result.Pooled.KeyObjects)
		require.Equal(t, 5, result.Pooled.NamedWithDirection)
		requireShare(t, 0.5, result.Pooled.KeyObjectsWithDirection)
	})

	t.Run("must_pass_class_only", func(t *testing.T) {
		setup(t, testMetrics)

		tests := []struct {
			image         int
			text          string
			withDirection int
			classOnly     int
		}{
			{image: 1, text: "Дверь и стол.", withDirection: 0, classOnly: 2},
			{image: 1, text: "Слева дверь.", withDirection: 0, classOnly: 1},
			{image: 2, text: "Здесь два стула.", withDirection: 0, classOnly: 2},
			{image: 3, text: "Справа машина.", withDirection: 1, classOnly: 1},
			{image: 3, text: "Справа шкаф.", withDirection: 0, classOnly: 0},
		}
		for _, test := range tests {
			totals := scorer.ScoreAnswer(test.text, images[test.image])
			require.Equal(t, test.withDirection, totals.NamedWithDirection, test.text)
			require.Equal(t, test.classOnly, totals.NamedClassOnly, test.text)
		}

		result := scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl")
		require.Equal(t, 8, result.Pooled.NamedClassOnly)
		requireShare(t, 0.8, result.Pooled.KeyObjectsClassOnly)
	})

	t.Run("must_pass_hallucinated", func(t *testing.T) {
		setup(t, testMetrics)

		tests := []struct {
			image        int
			text         string
			mentions     int
			hallucinated int
		}{
			{image: 1, text: "Впереди стол. Справа дверь.", mentions: 2, hallucinated: 0},
			{image: 1, text: "Впереди стол и машина.", mentions: 2, hallucinated: 1},    // no car on the snapshot
			{image: 1, text: "Слева окно, справа шкаф.", mentions: 0, hallucinated: 0},  // window and wardrobe are not key classes
			{image: 2, text: "Впереди стул. Слева стул.", mentions: 2, hallucinated: 0}, // a mention per sentence
			{image: 3, text: "Впереди человек.", mentions: 1, hallucinated: 0},          // annotated, not a key object
			{image: 3, text: "Впереди ступени и дверь.", mentions: 2, hallucinated: 2},
		}
		for _, test := range tests {
			totals := scorer.ScoreAnswer(test.text, images[test.image])
			require.Equal(t, test.mentions, totals.Mentions, test.text)
			require.Equal(t, test.hallucinated, totals.HallucinatedMentions, test.text)
		}

		empty := scorer.ScoreAnswer("Ничего не видно.", images[1])
		require.Nil(t, empty.Hallucinated)

		result := scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl")
		require.Equal(t, 9, result.Pooled.Mentions)
		require.Equal(t, 1, result.Pooled.HallucinatedMentions)
		requireShare(t, 1.0/9, result.Pooled.Hallucinated)
	})

	t.Run("must_pass_runs_and_union_spread", func(t *testing.T) {
		setup(t, testMetrics)

		result := scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl")

		require.Equal(t, []string{testConfigID}, result.ConfigIDs)
		require.Len(t, result.Runs, 2)

		first := result.Runs[0]
		require.Equal(t, 1, first.Run)
		require.Equal(t, 3, first.Answers) // ask and passive are not scored
		require.Equal(t, 0, first.Unanswered)
		require.Equal(t, 6, first.KeyObjects)
		requireShare(t, 4.0/6, first.KeyObjectsWithDirection)
		requireShare(t, 1.0, first.KeyObjectsClassOnly)
		requireShare(t, 0.0, first.Hallucinated)

		second := result.Runs[1]
		require.Equal(t, 2, second.Run)
		require.Equal(t, 2, second.Answers)
		require.Equal(t, 1, second.Unanswered) // the error is not scored
		require.Equal(t, 4, second.KeyObjects)
		requireShare(t, 1.0/4, second.KeyObjectsWithDirection)
		requireShare(t, 2.0/4, second.KeyObjectsClassOnly)
		requireShare(t, 1.0/3, second.Hallucinated)

		require.Equal(t, 5, result.Pooled.Answers)
		require.Equal(t, 1, result.Pooled.Unanswered)

		require.NotNil(t, result.Spread.KeyObjectsWithDirection)
		require.InDelta(t, 1.0/4, result.Spread.KeyObjectsWithDirection.Min, testDelta)
		require.InDelta(t, 4.0/6, result.Spread.KeyObjectsWithDirection.Max, testDelta)
		require.InDelta(t, (1.0/4+4.0/6)/2, result.Spread.KeyObjectsWithDirection.Mean, testDelta)
		require.InDelta(t, 0.5, result.Spread.KeyObjectsClassOnly.Min, testDelta)
		require.InDelta(t, 1.0, result.Spread.KeyObjectsClassOnly.Max, testDelta)
		require.InDelta(t, 0.0, result.Spread.Hallucinated.Min, testDelta)
		require.InDelta(t, 1.0/3, result.Spread.Hallucinated.Max, testDelta)
		require.Nil(t, result.Hypothesis)
	})

	t.Run("must_pass_per_image", func(t *testing.T) {
		setup(t, testMetrics)

		result := scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl")

		require.Len(t, result.Images, 3)
		expected := []struct {
			id, answers, unanswered, keyObjects, withDirection, classOnly, mentions, hallucinated int
			scene                                                                                 string
		}{
			{id: 1, scene: "indoor", answers: 2, keyObjects: 4, withDirection: 3, classOnly: 4, mentions: 4},
			{id: 2, scene: "indoor", answers: 2, keyObjects: 4, withDirection: 2, classOnly: 2, mentions: 2, hallucinated: 1},
			{id: 3, scene: "outdoor", answers: 1, unanswered: 1, keyObjects: 2, withDirection: 0, classOnly: 2, mentions: 3},
		}
		for i, want := range expected {
			image := result.Images[i]
			require.Equal(t, want.id, image.ImageID)
			require.Equal(t, want.scene, image.Scene)
			require.Equal(t, want.answers, image.Answers, "image %d", want.id)
			require.Equal(t, want.unanswered, image.Unanswered, "image %d", want.id)
			require.Equal(t, want.keyObjects, image.KeyObjects, "image %d", want.id)
			require.Equal(t, want.withDirection, image.NamedWithDirection, "image %d", want.id)
			require.Equal(t, want.classOnly, image.NamedClassOnly, "image %d", want.id)
			require.Equal(t, want.mentions, image.Mentions, "image %d", want.id)
			require.Equal(t, want.hallucinated, image.HallucinatedMentions, "image %d", want.id)
		}
		requireShare(t, 3.0/4, result.Images[0].KeyObjectsWithDirection)
	})

	t.Run("must_pass_multiple_jsonl_files", func(t *testing.T) {
		setup(t, testMetrics)

		args := []string{
			"--dataset", testMetrics,
			"--vocabulary", testVocabulary,
			filepath.Join(testMetrics, "run1.jsonl"),
			filepath.Join(testMetrics, "run2.jsonl"),
		}
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		require.Equal(t, ExitOK, Main(args, stdout, stderr), stderr.String())

		raw := map[string]any{}
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &raw))
		for _, key := range []string{"config_ids", "files", "runs", "pooled", "spread", "images"} {
			require.Contains(t, raw, key)
		}
		require.NotContains(t, raw, "hypothesis")

		result := &Result{}
		require.NoError(t, json.Unmarshal(stdout.Bytes(), result))
		require.Len(t, result.Files, 2)
		require.Len(t, result.Runs, 2)
		require.Equal(t, 10, result.Pooled.KeyObjects)
		require.Equal(t, scoreFiles(t, testMetrics, "run1.jsonl", "run2.jsonl"), result)
	})

	t.Run("must_fail_duplicate_run_numbers", func(t *testing.T) {
		setup(t, testMetrics)
		first := filepath.Join(testMetrics, "run2.jsonl")
		second := filepath.Join(testMetrics, "run2.duplicate.jsonl")

		_, err := ReadRecords([]string{filepath.Join(testMetrics, "run1.jsonl"), first, second})
		require.EqualError(t, err, fmt.Sprintf(errDuplicateRunFormat, 2, first, second))

		args := []string{"--dataset", testMetrics, "--vocabulary", testVocabulary, first, second}
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		require.Equal(t, ExitFailed, Main(args, stdout, stderr))
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Sprintf(errDuplicateRunFormat, 2, first, second)), stderr.String())
		require.Empty(t, stdout.String())
	})

	t.Run("must_pass_hypothesis_shape", func(t *testing.T) {
		setup(t, testVerdict)

		_, raw := runCommand(t, "quality90.jsonl", "stats.confirmed.json")
		hypothesis, ok := raw["hypothesis"].(map[string]any)
		require.True(t, ok)
		keys := []string{
			"config_id", "runs", "e2e_p95_ms", "e2e_p95_limit_ms", "key_objects_with_direction",
			"key_objects_limit", "llm_fallback_share", "llm_fallback_share_limit", "checks", "verdict",
		}
		require.Len(t, hypothesis, len(keys))
		for _, key := range keys {
			require.Contains(t, hypothesis, key)
		}
		require.Equal(t, testConfigID, hypothesis["config_id"])
		require.Equal(t, 5.0, hypothesis["runs"])
		require.Equal(t, 1500.0, hypothesis["e2e_p95_ms"])
		require.Equal(t, 2000.0, hypothesis["e2e_p95_limit_ms"])
		require.Equal(t, 0.9, hypothesis["key_objects_with_direction"])
		require.Equal(t, 0.75, hypothesis["key_objects_limit"])
		require.Equal(t, 0.05, hypothesis["llm_fallback_share"])
		require.Equal(t, 0.1, hypothesis["llm_fallback_share_limit"])
		require.Equal(t, map[string]any{"e2e": true, "quality": true, "fallbacks": true}, hypothesis["checks"])
		require.Equal(t, VerdictConfirmed, hypothesis["verdict"])

		_, raw = runCommand(t, "quality90.jsonl", "stats.no_e2e.json")
		hypothesis = raw["hypothesis"].(map[string]any)
		require.Len(t, hypothesis, len(keys))
		require.Contains(t, hypothesis, "e2e_p95_ms")
		require.Nil(t, hypothesis["e2e_p95_ms"])
		require.Equal(t, map[string]any{"e2e": nil, "quality": true, "fallbacks": true}, hypothesis["checks"])

		_, raw = runCommand(t, "quality90.jsonl", "stats.no_fallback_share.json")
		hypothesis = raw["hypothesis"].(map[string]any)
		require.Contains(t, hypothesis, "llm_fallback_share")
		require.Nil(t, hypothesis["llm_fallback_share"])
		require.Equal(t, map[string]any{"e2e": true, "quality": true, "fallbacks": nil}, hypothesis["checks"])
	})

	t.Run("must_pass_verdict_confirmed", func(t *testing.T) {
		setup(t, testVerdict)

		// p95 1500 ms, 90 %, fallbacks 5 %
		result, _ := runCommand(t, "quality90.jsonl", "stats.confirmed.json")
		h := result.Hypothesis
		require.Equal(t, 5, h.Runs)
		require.Equal(t, 1500.0, *h.E2EP95MS)
		requireShare(t, 0.9, h.KeyObjectsWithDirection)
		requireShare(t, 0.05, h.LLMFallbackShare)
		requireChecks(t, h, yes, yes, yes)
		require.Equal(t, VerdictConfirmed, h.Verdict)

		// exactly on the limits: 2000 ms, 75 %, 10 %
		result, _ = runCommand(t, "quality75.jsonl", "stats.boundary.json")
		h = result.Hypothesis
		require.Equal(t, 2000.0, *h.E2EP95MS)
		require.Equal(t, 75, result.Pooled.NamedWithDirection)
		require.Equal(t, 100, result.Pooled.KeyObjects)
		requireShare(t, 0.1, h.LLMFallbackShare)
		requireChecks(t, h, yes, yes, yes)
		require.Equal(t, VerdictConfirmed, h.Verdict)
	})

	t.Run("must_fail_verdict_fallbacks_over_limit", func(t *testing.T) {
		setup(t, testVerdict)

		// p95 1500 ms, 90 %, fallbacks 12 %
		result, _ := runCommand(t, "quality90.jsonl", "stats.fallbacks_over.json")
		h := result.Hypothesis
		requireShare(t, 0.12, h.LLMFallbackShare)
		requireChecks(t, h, yes, yes, no)
		require.Equal(t, VerdictNotConfirmed, h.Verdict)
	})

	t.Run("must_fail_verdict_e2e_over_limit", func(t *testing.T) {
		setup(t, testVerdict)

		// p95 2001 ms, 90 %, fallbacks 5 %
		result, _ := runCommand(t, "quality90.jsonl", "stats.e2e_over.json")
		h := result.Hypothesis
		require.Equal(t, 2001.0, *h.E2EP95MS)
		requireChecks(t, h, no, yes, yes)
		require.Equal(t, VerdictNotConfirmed, h.Verdict)
	})

	t.Run("must_fail_verdict_quality_under_limit", func(t *testing.T) {
		setup(t, testVerdict)

		// p95 1500 ms, 74 %, fallbacks 5 %
		result, _ := runCommand(t, "quality74.jsonl", "stats.confirmed.json")
		h := result.Hypothesis
		require.Equal(t, 74, result.Pooled.NamedWithDirection)
		require.Equal(t, 100, result.Pooled.KeyObjects)
		requireShare(t, 0.74, h.KeyObjectsWithDirection)
		requireChecks(t, h, yes, no, yes)
		require.Equal(t, VerdictNotConfirmed, h.Verdict)
	})

	t.Run("must_fail_verdict_insufficient_data", func(t *testing.T) {
		setup(t, testVerdict)

		// no E2E of describe in /v1/stats
		result, _ := runCommand(t, "quality90.jsonl", "stats.no_e2e.json")
		require.Nil(t, result.Hypothesis.E2EP95MS)
		requireChecks(t, result.Hypothesis, nil, yes, yes)
		require.Equal(t, VerdictInsufficientData, result.Hypothesis.Verdict)

		// no llm_fallback_share in /v1/stats
		result, _ = runCommand(t, "quality90.jsonl", "stats.no_fallback_share.json")
		require.Nil(t, result.Hypothesis.LLMFallbackShare)
		requireChecks(t, result.Hypothesis, yes, yes, nil)
		require.Equal(t, VerdictInsufficientData, result.Hypothesis.Verdict)

		response, err := loadStats(filepath.Join(testVerdict, "stats.confirmed.json"))
		require.NoError(t, err)
		input, err := ReadRecords([]string{filepath.Join(testVerdict, "quality90.jsonl")})
		require.NoError(t, err)

		// no scored answer: no key objects
		empty, err := scorer.Score(&Input{})
		require.NoError(t, err)
		h, err := BuildHypothesis(empty, response)
		require.NoError(t, err)
		require.Nil(t, h.KeyObjectsWithDirection)
		requireChecks(t, h, yes, nil, yes)
		require.Equal(t, VerdictInsufficientData, h.Verdict)

		// fewer than three runs
		twoRuns := &Input{}
		for _, entry := range input.Entries {
			if entry.Record.Run <= 2 {
				twoRuns.Entries = append(twoRuns.Entries, entry)
			}
		}
		result, err = scorer.Score(twoRuns)
		require.NoError(t, err)
		h, err = BuildHypothesis(result, response)
		require.NoError(t, err)
		require.Equal(t, 2, h.Runs)
		requireChecks(t, h, yes, yes, yes)
		require.Equal(t, VerdictInsufficientData, h.Verdict)

		// a failed check outweighs a missing value
		over, err := loadStats(filepath.Join(testVerdict, "stats.e2e_over.json"))
		require.NoError(t, err)
		h, err = BuildHypothesis(empty, over)
		require.NoError(t, err)
		requireChecks(t, h, no, nil, yes)
		require.Equal(t, VerdictNotConfirmed, h.Verdict)

		// /v1/stats of another configuration
		other := "000000000000"
		response.ConfigID = &other
		result, err = scorer.Score(input)
		require.NoError(t, err)
		_, err = BuildHypothesis(result, response)
		require.EqualError(t, err, fmt.Sprintf(errConfigMismatchFormat, testConfigID, other))
	})

	t.Run("must_fail_inputs", func(t *testing.T) {
		setup(t, testMetrics)

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		require.Equal(t, ExitUsage, Main([]string{"--dataset", testMetrics}, stdout, stderr))
		require.Equal(t, fmt.Sprintf(diagErrorFormat, errNoFiles), stderr.String())

		missing := filepath.Join(t.TempDir(), "missing.jsonl")
		_, err := ReadRecords([]string{missing})
		require.ErrorContains(t, err, fmt.Sprintf("failed to read %q", missing))

		unknown := 7
		input := &Input{Entries: []Entry{{File: "run.jsonl", Line: 3}}}
		input.Entries[0].Record.Run = 1
		input.Entries[0].Record.Kind = "describe"
		input.Entries[0].Record.ImageID = &unknown
		_, err = scorer.Score(input)
		require.EqualError(t, err, fmt.Sprintf(errUnknownImageFormat, "run.jsonl", 3, unknown))
	})
}
