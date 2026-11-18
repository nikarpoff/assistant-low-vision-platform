package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/configs"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/textnorm"
)

// normalizeCasesPath is the table of the normalization cases shared with the ASR service.
const normalizeCasesPath = "../../testdata/contracts/asr-service/normalize.cases.json"

// defaultPhrases are the phrases of the commands by the table of PLT-4.
var defaultPhrases = map[protocol.Intent][]string{
	protocol.IntentDescribe:      {"опиши", "опиши обстановку", "что вокруг", "что ты видишь", "что видишь", "осмотрись"},
	protocol.IntentDescribeAhead: {"что впереди", "что передо мной", "что спереди"},
	protocol.IntentRepeat:        {"повтори", "еще раз", "не расслышал", "не понял", "что ты сказал"},
	protocol.IntentStop:          {"стоп", "хватит", "замолчи", "тихо"},
	protocol.IntentPassiveOn:     {"включи пассивный режим", "следи за дорогой", "предупреждай об опасности"},
	protocol.IntentPassiveOff:    {"выключи пассивный режим", "не следи", "перестань следить"},
}

// caseIntents are the intents of the cases of the normalization table; empty - the text is empty.
var caseIntents = map[string]protocol.Intent{
	"command_with_period": protocol.IntentDescribe,
	"command_two_words":   protocol.IntentDescribe,
	"question_mark":       protocol.IntentDescribeAhead,
	"yo_lowercase":        protocol.IntentRepeat,
	"yo_uppercase":        protocol.IntentRepeat,
	"upper_case":          protocol.IntentPassiveOff,
	"please_at_end":       protocol.IntentRepeat,
	"please_at_start":     protocol.IntentDescribe,
	"please_both_commas":  protocol.IntentPassiveOff,
	"question_where":      protocol.IntentQuestion,
	"question_people":     protocol.IntentQuestion,
	"hyphen_inside_word":  protocol.IntentQuestion,
	"hyphen_what":         protocol.IntentQuestion,
	"em_dash":             protocol.IntentQuestion,
	"ellipsis_char":       protocol.IntentDescribe,
	"three_dots":          protocol.IntentRepeat,
	"repeated_marks":      protocol.IntentStop,
	"colon":               protocol.IntentQuestion,
	"semicolon":           protocol.IntentQuestion,
	"guillemets":          protocol.IntentQuestion,
	"double_quotes":       protocol.IntentQuestion,
	"parentheses_and_yo":  protocol.IntentQuestion,
	"underscore":          protocol.IntentDescribeAhead,
	"digits_kept":         protocol.IntentQuestion,
	"symbol_kept":         protocol.IntentQuestion,
	"latin":               protocol.IntentQuestion,
	"outer_spaces":        protocol.IntentStop,
	"tab_and_spaces":      protocol.IntentPassiveOn,
	"newline":             protocol.IntentDescribeAhead,
	"no_break_space":      protocol.IntentQuestion,
	"only_punctuation":    "",
	"only_spaces":         "",
	"empty":               "",
}

func TestIntent(t *testing.T) {
	var (
		matcher *Matcher
		dir     string
	)

	setup := func(t *testing.T) {
		var err error
		matcher, err = LoadMatcher("")
		require.NoError(t, err)
		dir = t.TempDir()
	}

	// writeFile writes an intents file into the temporary directory and returns its path.
	writeFile := func(t *testing.T, content string) string {
		path := filepath.Join(dir, "intents.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	// recognize returns the intent of the text and fails the test on an error.
	recognize := func(t *testing.T, text string) protocol.Intent {
		intent, err := matcher.Recognize(text)
		require.NoError(t, err, text)
		return intent
	}

	t.Run("must_pass_default_phrases", func(t *testing.T) {
		setup(t)

		recognized := map[protocol.Intent][]string{}
		for _, phrases := range defaultPhrases {
			for _, phrase := range phrases {
				got := recognize(t, phrase)
				recognized[got] = append(recognized[got], phrase)
			}
		}

		require.Equal(t, defaultPhrases, recognized)
		require.Len(t, matcher.phrases, 24)
	})

	t.Run("must_pass_please_at_start_and_end", func(t *testing.T) {
		setup(t)

		for intent, phrases := range defaultPhrases {
			for _, phrase := range phrases {
				require.Equal(t, intent, recognize(t, "Пожалуйста, "+phrase), phrase)
				require.Equal(t, intent, recognize(t, phrase+", пожалуйста."), phrase)
				require.Equal(t, intent, recognize(t, "пожалуйста "+phrase+" пожалуйста"), phrase)
			}
		}
		require.Equal(t, protocol.IntentQuestion, recognize(t, "Пожалуйста"))
		require.Equal(t, protocol.IntentQuestion, recognize(t, "Пожалуйста, пожалуйста"))
		require.Equal(t, protocol.IntentQuestion, recognize(t, "Опиши, пожалуйста, подробно"))
	})

	t.Run("must_pass_yo", func(t *testing.T) {
		setup(t)

		require.Equal(t, protocol.IntentRepeat, recognize(t, "Ещё раз"))
		require.Equal(t, protocol.IntentRepeat, recognize(t, "ЕЩЁ РАЗ!"))
		require.Equal(t, protocol.IntentRepeat, recognize(t, "Ещё раз, пожалуйста"))
	})

	t.Run("must_pass_punctuation_and_case", func(t *testing.T) {
		setup(t)

		require.Equal(t, protocol.IntentDescribe, recognize(t, "Опиши."))
		require.Equal(t, protocol.IntentDescribe, recognize(t, "Что ты видишь…"))
		require.Equal(t, protocol.IntentDescribeAhead, recognize(t, "Что впереди?"))
		require.Equal(t, protocol.IntentDescribeAhead, recognize(t, "Что_передо_мной"))
		require.Equal(t, protocol.IntentDescribeAhead, recognize(t, "Что спереди?\n"))
		require.Equal(t, protocol.IntentRepeat, recognize(t, "Не понял..."))
		require.Equal(t, protocol.IntentStop, recognize(t, "Тихо!!!"))
		require.Equal(t, protocol.IntentStop, recognize(t, "  Стоп  "))
		require.Equal(t, protocol.IntentPassiveOn, recognize(t, "Включи   пассивный\tрежим"))
		require.Equal(t, protocol.IntentPassiveOff, recognize(t, "ВЫКЛЮЧИ ПАССИВНЫЙ РЕЖИМ"))
		require.Equal(t, protocol.IntentPassiveOff, recognize(t, "Перестань следить, пожалуйста!"))
	})

	t.Run("must_pass_question", func(t *testing.T) {
		setup(t)

		for _, text := range []string{
			"Где дверь?",
			"В комнате есть люди?",
			"Где-то есть дверь?",
			"Что-то впереди есть?",
			"Хватит — замолчи",
			"Скажи «стоп»",
			"Опиши обстановку подробно",
			"стопка",
			"OK, Stop",
		} {
			require.Equal(t, protocol.IntentQuestion, recognize(t, text), text)
		}
	})

	t.Run("must_pass_normalize_cases", func(t *testing.T) {
		setup(t)
		body, err := os.ReadFile(normalizeCasesPath)
		require.NoError(t, err)
		table := struct {
			Cases []struct {
				Name     string `json:"name"`
				Input    string `json:"input"`
				Expected string `json:"expected"`
			} `json:"cases"`
		}{}
		require.NoError(t, json.Unmarshal(body, &table))
		require.GreaterOrEqual(t, len(table.Cases), 20)

		for _, tc := range table.Cases {
			t.Run(fmt.Sprintf("must_pass_case_%s", tc.Name), func(t *testing.T) {
				normalized := textnorm.Normalize(tc.Input)
				intent, err := matcher.Recognize(tc.Input)
				normalizedIntent, normalizedErr := matcher.Recognize(tc.Expected)

				require.Equal(t, tc.Expected, normalized)
				require.Equal(t, normalizedIntent, intent)
				require.Equal(t, normalizedErr, err)
				expected, ok := caseIntents[tc.Name]
				if !ok {
					return
				}
				require.Equal(t, expected, intent)
				if expected == "" {
					require.ErrorIs(t, err, ErrEmptyText)
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("must_pass_custom_file", func(t *testing.T) {
		setup(t)
		path := writeFile(t, "describe:\n  - Что там?\n  - опиши\nstop:\n  - Довольно!\n")

		custom, err := LoadMatcher(path)

		require.NoError(t, err)
		describe, err := custom.Recognize("что там")
		require.NoError(t, err)
		require.Equal(t, protocol.IntentDescribe, describe)
		stop, err := custom.Recognize("Довольно, пожалуйста")
		require.NoError(t, err)
		require.Equal(t, protocol.IntentStop, stop)
		repeat, err := custom.Recognize("повтори")
		require.NoError(t, err)
		require.Equal(t, protocol.IntentQuestion, repeat)
		require.NotEqual(t, matcher.Hash(), custom.Hash())
	})

	t.Run("must_pass_hash", func(t *testing.T) {
		setup(t)
		sum := sha256.Sum256(configs.Intents)

		embedded, err := NewMatcher(configs.Intents)

		require.NoError(t, err)
		require.Equal(t, hex.EncodeToString(sum[:])[:12], matcher.Hash())
		require.Equal(t, matcher.Hash(), embedded.Hash())
		fromFile, err := LoadMatcher("../../configs/intents.yaml")
		require.NoError(t, err)
		require.Equal(t, matcher.Hash(), fromFile.Hash())
	})

	t.Run("must_fail_empty_text", func(t *testing.T) {
		setup(t)

		for _, text := range []string{"", "   ", "?!..", "\n"} {
			intent, err := matcher.Recognize(text)

			require.Equal(t, protocol.Intent(""), intent)
			require.ErrorIs(t, err, ErrEmptyText)
			require.EqualError(t, err, "empty text")
		}
	})

	t.Run("must_fail_unknown_intent", func(t *testing.T) {
		setup(t)

		custom, err := NewMatcher([]byte("describe: [опиши]\nquestion: [что это]\n"))

		require.Nil(t, custom)
		require.EqualError(t, err, `failed to parse intents: unknown intent "question": allowed values are describe, describe_ahead, repeat, stop, passive_on, passive_off`)
	})

	t.Run("must_fail_duplicate_phrase", func(t *testing.T) {
		setup(t)

		custom, err := NewMatcher([]byte("stop: [Хватит]\ndescribe: [хватит!]\n"))

		require.Nil(t, custom)
		require.EqualError(t, err, `failed to parse intents: phrase "хватит" belongs to intents "describe" and "stop"`)
	})

	t.Run("must_fail_empty_phrase", func(t *testing.T) {
		setup(t)

		custom, err := NewMatcher([]byte("stop: [стоп, \"?!\"]\n"))

		require.Nil(t, custom)
		require.EqualError(t, err, `failed to parse intents: intent "stop": phrase "?!" is empty after normalization`)
	})

	t.Run("must_fail_no_phrases", func(t *testing.T) {
		setup(t)

		custom, err := NewMatcher([]byte("# only a comment\n"))

		require.Nil(t, custom)
		require.EqualError(t, err, "failed to parse intents: no phrases")
	})

	t.Run("must_fail_invalid_yaml", func(t *testing.T) {
		setup(t)

		custom, err := NewMatcher([]byte("stop: [стоп\n"))

		require.Nil(t, custom)
		require.EqualError(t, err, "failed to parse intents: yaml: line 1: did not find expected ',' or ']'")
	})

	t.Run("must_fail_file_not_found", func(t *testing.T) {
		setup(t)
		path := filepath.Join(dir, "missing.yaml")

		custom, err := LoadMatcher(path)

		require.Nil(t, custom)
		require.EqualError(t, err, fmt.Sprintf("failed to read intents file: open %s: no such file or directory", path))
	})
}
