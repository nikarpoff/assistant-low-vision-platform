package vocabulary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const contractPath = "../../testdata/contracts/objects-detector/vocabulary.response.json"

// readDocument reads the vocabulary example of the detector contract.
func readDocument(t *testing.T) *Document {
	t.Helper()
	body, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	document := &Document{}
	require.NoError(t, json.Unmarshal(body, document))
	return document
}

func TestVocabulary(t *testing.T) {
	var (
		document *Document
		source   *sourceMock
		loader   *Loader
		pauses   []time.Duration
		failures int
		getLogs  func() string
	)

	setup := func(t *testing.T) {
		getLogs = logger.InitTestLogger()
		document = readDocument(t)
		pauses = nil
		failures = 0
		source = &sourceMock{
			VocabularyFunc: func(ctx context.Context) (*Document, error) {
				if failures > 0 {
					failures--
					return nil, errors.New("upstream unavailable: upstream=detector status=500")
				}
				return document, nil
			},
		}
		loader = NewLoader(source, 10*time.Millisecond, 25*time.Millisecond)
		loader.wait = func(ctx context.Context, pause time.Duration) error {
			pauses = append(pauses, pause)
			return ctx.Err()
		}
	}

	t.Run("must_pass_contract", func(t *testing.T) {
		setup(t)

		vocabulary, err := NewVocabulary(document)

		require.NoError(t, err)
		require.Equal(t, "8778f20c71d0", vocabulary.Hash())
		require.Len(t, vocabulary.Classes(), 26)
		keys := []string{}
		for _, class := range vocabulary.Classes() {
			if class.Key {
				keys = append(keys, class.ID)
			}
		}
		require.Equal(t, []string{
			"person", "door", "stairs", "car", "bus", "truck", "bicycle", "motorcycle",
			"scooter", "traffic_light", "crosswalk", "pole", "bench", "barrier", "chair", "table",
		}, keys)
		for _, id := range []string{"bed", "wardrobe", "sofa", "window", "sink", "toilet", "refrigerator", "tv", "dog", "trash_can"} {
			require.True(t, vocabulary.Has(id), id)
		}
		chair, ok := vocabulary.Class("chair")
		require.True(t, ok)
		require.Equal(t, Forms{One: "стул", Few: "стула", Many: "стульев", Gender: GenderMasculine}, chair.RU)
		_, ok = vocabulary.Class("unicorn")
		require.False(t, ok)
		require.False(t, vocabulary.Has("unicorn"))
	})

	t.Run("must_pass_forms", func(t *testing.T) {
		setup(t)
		vocabulary, err := NewVocabulary(document)
		require.NoError(t, err)
		chair, _ := vocabulary.Class("chair")
		door, _ := vocabulary.Class("door")
		stairs, _ := vocabulary.Class("stairs")

		forms := map[int]string{}
		for _, count := range []int{0, 1, 2, 3, 4, 5, 11, 12, 14, 21, 22, 25, 101, 111, 112, -2} {
			forms[count] = chair.RU.ForCount(count)
		}

		require.Equal(t, map[int]string{
			0: "стульев", 1: "стул", 2: "стула", 3: "стула", 4: "стула", 5: "стульев",
			11: "стульев", 12: "стульев", 14: "стульев", 21: "стул", 22: "стула", 25: "стульев",
			101: "стул", 111: "стульев", 112: "стульев", -2: "стула",
		}, forms)
		require.Equal(t, "двери", door.RU.ForCount(2))
		require.Equal(t, "дверей", door.RU.ForCount(5))
		require.Equal(t, GenderFeminine, door.RU.Gender)
		require.Equal(t, GenderPlural, stairs.RU.Gender)
		require.Equal(t, PluralOne, PluralOf(31))
		require.Equal(t, PluralFew, PluralOf(43))
		require.Equal(t, PluralMany, PluralOf(13))
	})

	t.Run("must_pass_mentions", func(t *testing.T) {
		setup(t)
		vocabulary, err := NewVocabulary(document)
		require.NoError(t, err)

		mentions := map[string][]string{}
		for _, text := range []string{
			"впереди две двери и стул слева",
			"у стола стоят стулья",
			"впереди столб",
			"на улице двое людей и собака",
			"слева электросамокат справа пешеходный переход",
			"осторожно впереди близко ступени",
			"дверной проем впереди",
			"стульчик",
			"",
		} {
			mentions[text] = vocabulary.Mentions(text)
		}

		require.Equal(t, map[string][]string{
			"впереди две двери и стул слева":                 {"door", "chair"},
			"у стола стоят стулья":                           {"chair", "table"},
			"впереди столб":                                  {"pole"},
			"на улице двое людей и собака":                   {"person", "dog"},
			"слева электросамокат справа пешеходный переход": {"scooter", "crosswalk"},
			"осторожно впереди близко ступени":               {"stairs"},
			"дверной проем впереди":                          {},
			"стульчик": {},
			"":         {},
		}, mentions)
		require.True(t, vocabulary.Mentioned("door", "дверь"))
		require.False(t, vocabulary.Mentioned("door", "дверной"))
		require.False(t, vocabulary.Mentioned("unicorn", "дверь"))
	})

	t.Run("must_fail_no_classes", func(t *testing.T) {
		setup(t)

		vocabulary, err := NewVocabulary(&Document{Hash: "000000000000"})

		require.Nil(t, vocabulary)
		require.EqualError(t, err, "vocabulary has no classes")
	})

	t.Run("must_fail_duplicate_class", func(t *testing.T) {
		setup(t)
		document.Classes = append(document.Classes, document.Classes[1])

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, `duplicate class "door"`)
	})

	t.Run("must_fail_invalid_id", func(t *testing.T) {
		setup(t)
		document.Classes[0].ID = "Traffic Light"

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, `invalid class id "Traffic Light": lowercase latin snake_case is required`)
	})

	t.Run("must_fail_missing_forms", func(t *testing.T) {
		setup(t)
		document.Classes[1].RU.Many = ""

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, `class "door": forms one, few and many are required`)
	})

	t.Run("must_fail_unknown_gender", func(t *testing.T) {
		setup(t)
		document.Classes[1].RU.Gender = "x"

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, `class "door": unknown gender "x": allowed values are m, f, n, pl`)
	})

	t.Run("must_fail_invalid_mention", func(t *testing.T) {
		setup(t)
		document.Classes[1].Mentions = []string{"двер(ь"}

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, "class \"door\": invalid mention \"двер(ь\": error parsing regexp: missing closing ): `(?:^| )(?:двер(ь)(?: |$)`")
	})

	t.Run("must_fail_lookaround_mention", func(t *testing.T) {
		setup(t)
		document.Classes[1].Mentions = []string{"двер(?=ь)"}

		vocabulary, err := NewVocabulary(document)

		require.Nil(t, vocabulary)
		require.EqualError(t, err, "class \"door\": invalid mention \"двер(?=ь)\": error parsing regexp: invalid or unsupported Perl syntax: `(?=`")
	})

	t.Run("must_pass_loader", func(t *testing.T) {
		setup(t)

		require.False(t, loader.Loaded())
		require.Nil(t, loader.Vocabulary())
		vocabulary, err := loader.Run(context.Background())

		require.NoError(t, err)
		require.Equal(t, "8778f20c71d0", vocabulary.Hash())
		require.True(t, loader.Loaded())
		require.Same(t, vocabulary, loader.Vocabulary())
		require.Len(t, source.VocabularyCalls(), 1)
		require.Empty(t, pauses)
		require.Contains(t, getLogs(), "INFO: Vocabulary loaded: hash=8778f20c71d0 classes=26 attempts=1")
	})

	t.Run("must_pass_loader_retries_with_growing_pause", func(t *testing.T) {
		setup(t)
		failures = 4

		vocabulary, err := loader.Run(context.Background())

		require.NoError(t, err)
		require.NotNil(t, vocabulary)
		require.True(t, loader.Loaded())
		require.Len(t, source.VocabularyCalls(), 5)
		require.Equal(t, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond, 25 * time.Millisecond}, pauses)
		require.Contains(t, getLogs(), "WARNING: failed to load vocabulary, retrying: attempt=1 pause=10ms error=upstream unavailable: upstream=detector status=500")
		require.Contains(t, getLogs(), "WARNING: failed to load vocabulary, retrying: attempt=3 pause=25ms error=upstream unavailable: upstream=detector status=500")
		require.Contains(t, getLogs(), "INFO: Vocabulary loaded: hash=8778f20c71d0 classes=26 attempts=5")
	})

	t.Run("must_pass_loader_retries_invalid_vocabulary", func(t *testing.T) {
		setup(t)
		valid := document
		calls := 0
		source.VocabularyFunc = func(ctx context.Context) (*Document, error) {
			calls++
			if calls == 1 {
				return &Document{Hash: "000000000000"}, nil
			}
			return valid, nil
		}

		vocabulary, err := loader.Run(context.Background())

		require.NoError(t, err)
		require.Equal(t, "8778f20c71d0", vocabulary.Hash())
		require.Equal(t, []time.Duration{10 * time.Millisecond}, pauses)
		require.Contains(t, getLogs(), "WARNING: failed to load vocabulary, retrying: attempt=1 pause=10ms error=vocabulary has no classes")
	})

	t.Run("must_fail_loader_cancelled", func(t *testing.T) {
		setup(t)
		failures = 1000
		ctx, cancel := context.WithCancel(context.Background())
		loader.wait = func(ctx context.Context, pause time.Duration) error {
			pauses = append(pauses, pause)
			if len(pauses) == 3 {
				cancel()
			}
			return ctx.Err()
		}

		vocabulary, err := loader.Run(ctx)

		require.Nil(t, vocabulary)
		require.ErrorIs(t, err, context.Canceled)
		require.EqualError(t, err, "vocabulary not loaded: context canceled")
		require.False(t, loader.Loaded())
		require.Nil(t, loader.Vocabulary())
		require.Len(t, source.VocabularyCalls(), 3)
	})

	t.Run("must_pass_sleep", func(t *testing.T) {
		setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		waited := sleep(context.Background(), time.Millisecond)
		cancelled := sleep(ctx, time.Hour)

		require.NoError(t, waited)
		require.EqualError(t, cancelled, fmt.Sprint(context.Canceled))
	})
}
