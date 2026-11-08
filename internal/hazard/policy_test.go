package hazard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/configs"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Paths of the contracts.
const (
	vocabularyPath     = "../../testdata/contracts/objects-detector/vocabulary.response.json"
	prewarmPhrasesPath = "../../testdata/contracts/tts-service/prewarm_phrases.txt"
)

// readVocabulary reads the vocabulary example of the detector contract.
func readVocabulary(t *testing.T) *vocabulary.Vocabulary {
	t.Helper()
	body, err := os.ReadFile(vocabularyPath)
	require.NoError(t, err)
	document := &vocabulary.Document{}
	require.NoError(t, json.Unmarshal(body, document))
	v, err := vocabulary.NewVocabulary(document)
	require.NoError(t, err)
	return v
}

// object builds a detected object.
func object(label string, direction detector.Direction, distance detector.Distance, confidence, area float64) detector.Object {
	return detector.Object{Label: label, Direction: direction, Distance: distance, Confidence: confidence, Area: area}
}

func TestHazardPolicy(t *testing.T) {
	var (
		vocab    *vocabulary.Vocabulary
		policy   *Policy
		cooldown *Cooldown
		now      time.Time
		dir      string
	)

	setup := func(t *testing.T) {
		vocab = readVocabulary(t)
		rules, err := LoadRules("")
		require.NoError(t, err)
		policy, err = NewPolicy(rules, vocab)
		require.NoError(t, err)
		now = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		cooldown = NewCooldown(10*time.Second, func() time.Time { return now })
		dir = t.TempDir()
	}

	// writeFile writes a rules file into the temporary directory and returns its path.
	writeFile := func(t *testing.T, content string) string {
		path := filepath.Join(dir, "hazard_rules.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	// phrases returns the phrases of the matches.
	phrases := func(matches []Match) []string {
		result := []string{}
		for _, match := range matches {
			result = append(result, match.Phrase)
		}
		return result
	}

	t.Run("must_pass_default_rules", func(t *testing.T) {
		setup(t)
		cases := []struct {
			object detector.Object
			rule   string
			level  Level
			phrase string
		}{
			{object("car", "left", "near", 0.9, 0.2), "vehicle_near", LevelDanger, "Осторожно, слева близко машина"},
			{object("bus", "ahead", "near", 0.9, 0.2), "vehicle_near", LevelDanger, "Осторожно, впереди близко автобус"},
			{object("truck", "right", "near", 0.9, 0.2), "vehicle_near", LevelDanger, "Осторожно, справа близко грузовик"},
			{object("motorcycle", "ahead", "near", 0.9, 0.2), "vehicle_near", LevelDanger, "Осторожно, впереди близко мотоцикл"},
			{object("car", "ahead", "far", 0.9, 0.05), "vehicle_ahead_far", LevelNotice, "Впереди машина"},
			{object("bicycle", "ahead", "near", 0.9, 0.2), "two_wheeler_ahead_near", LevelDanger, "Осторожно, впереди велосипед"},
			{object("scooter", "ahead", "near", 0.9, 0.2), "two_wheeler_ahead_near", LevelDanger, "Осторожно, впереди самокат"},
			{object("stairs", "ahead", "near", 0.81, 0.189), "stairs_ahead_near", LevelDanger, "Осторожно, впереди ступени"},
			{object("pole", "ahead", "near", 0.9, 0.2), "obstacle_ahead_near", LevelDanger, "Осторожно, впереди столб"},
			{object("barrier", "ahead", "near", 0.9, 0.2), "obstacle_ahead_near", LevelDanger, "Осторожно, впереди ограждение"},
			{object("bench", "ahead", "near", 0.9, 0.2), "obstacle_ahead_near", LevelDanger, "Осторожно, впереди скамейка"},
			{object("trash_can", "ahead", "near", 0.9, 0.2), "obstacle_ahead_near", LevelDanger, "Осторожно, впереди урна"},
			{object("person", "ahead", "near", 0.9, 0.2), "person_ahead_near", LevelNotice, "Впереди близко человек"},
			{object("crosswalk", "ahead", "far", 0.9, 0.2), "crossing_ahead", LevelNotice, "Впереди пешеходный переход"},
			{object("traffic_light", "ahead", "near", 0.9, 0.2), "crossing_ahead", LevelNotice, "Впереди светофор"},
		}
		misses := []detector.Object{
			object("car", "left", "far", 0.9, 0.05),
			object("bicycle", "left", "near", 0.9, 0.2),
			object("stairs", "ahead", "far", 0.9, 0.2),
			object("stairs", "right", "near", 0.9, 0.2),
			object("pole", "left", "near", 0.9, 0.2),
			object("person", "ahead", "far", 0.9, 0.2),
			object("person", "left", "near", 0.9, 0.2),
			object("crosswalk", "left", "near", 0.9, 0.2),
			object("chair", "ahead", "near", 0.9, 0.2),
			object("door", "ahead", "near", 0.9, 0.2),
			object("stairs", "ahead", "near", 0.3, 0.2),
		}

		for _, tc := range cases {
			match, ok := policy.Match(tc.object)

			require.True(t, ok, tc.phrase)
			require.Equal(t, Match{Object: tc.object, Rule: tc.rule, Level: tc.level, Phrase: tc.phrase}, match)
		}
		for _, miss := range misses {
			_, ok := policy.Match(miss)
			require.False(t, ok, miss)
		}
		require.Len(t, policy.rules, 7)
	})

	t.Run("must_pass_levels", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{
			object("person", "ahead", "near", 0.9, 0.30),
			object("chair", "left", "near", 0.9, 0.25),
			object("car", "ahead", "far", 0.9, 0.20),
			object("crosswalk", "ahead", "near", 0.9, 0.10),
			object("pole", "ahead", "near", 0.9, 0.05),
			object("stairs", "ahead", "near", 0.9, 0.15),
		}

		matches := policy.Evaluate(objects)

		require.Equal(t, []string{
			"Осторожно, впереди ступени",
			"Осторожно, впереди столб",
			"Впереди близко человек",
			"Впереди пешеходный переход",
			"Впереди машина",
		}, phrases(matches))
		require.Equal(t, []Level{LevelDanger, LevelDanger, LevelNotice, LevelNotice, LevelNotice},
			[]Level{matches[0].Level, matches[1].Level, matches[2].Level, matches[3].Level, matches[4].Level})
		require.Empty(t, policy.Evaluate([]detector.Object{object("chair", "left", "near", 0.9, 0.25)}))

		rules, err := ParseRules([]byte(`rules:
  - name: notice_first
    classes: [car]
    level: notice
    phrase: "Рядом {object}"
  - name: danger_second
    classes: [car]
    distances: [near]
    level: danger
    phrase: "Осторожно, {direction} {object}"
`))
		require.NoError(t, err)
		overlapping, err := NewPolicy(rules, vocab)
		require.NoError(t, err)
		near, ok := overlapping.Match(object("car", "right", "near", 0.9, 0.2))
		require.True(t, ok)
		require.Equal(t, "danger_second", near.Rule)
		require.Equal(t, LevelDanger, near.Level)
		require.Equal(t, "Осторожно, справа машина", near.Phrase)
		far, ok := overlapping.Match(object("car", "right", "far", 0.9, 0.2))
		require.True(t, ok)
		require.Equal(t, "notice_first", far.Rule)
		require.Equal(t, LevelNotice, far.Level)
	})

	t.Run("must_pass_cooldown", func(t *testing.T) {
		setup(t)
		carAhead := []detector.Object{object("car", "ahead", "near", 0.9, 0.3)}
		frame := func(objects ...detector.Object) []string {
			return phrases(cooldown.Apply(policy.Evaluate(objects), objects))
		}

		first := frame(carAhead...)
		now = now.Add(3 * time.Second)
		repeated := frame(carAhead...)
		otherDirection := frame(object("car", "left", "near", 0.9, 0.3))
		otherClass := frame(object("bus", "ahead", "near", 0.9, 0.3))
		now = now.Add(6999 * time.Millisecond)
		beforePeriod := frame(carAhead...)
		now = now.Add(time.Millisecond)
		afterPeriod := frame(carAhead...)
		twoOfPair := frame(object("pole", "ahead", "near", 0.9, 0.3), object("pole", "ahead", "near", 0.8, 0.1))

		require.Equal(t, []string{"Осторожно, впереди близко машина"}, first)
		require.Empty(t, repeated)
		require.Equal(t, []string{"Осторожно, слева близко машина"}, otherDirection)
		require.Equal(t, []string{"Осторожно, впереди близко автобус"}, otherClass)
		require.Empty(t, beforePeriod)
		require.Equal(t, []string{"Осторожно, впереди близко машина"}, afterPeriod)
		require.Equal(t, []string{"Осторожно, впереди столб"}, twoOfPair)
	})

	t.Run("must_pass_far_to_near", func(t *testing.T) {
		setup(t)
		near := object("car", "ahead", "near", 0.9, 0.3)
		far := object("car", "ahead", "far", 0.9, 0.05)
		frame := func(objects ...detector.Object) []string {
			return phrases(cooldown.Apply(policy.Evaluate(objects), objects))
		}

		notice := frame(far)
		now = now.Add(3 * time.Second)
		approached := frame(near)
		now = now.Add(3 * time.Second)
		stillNear := frame(near)
		now = now.Add(time.Second)
		movedAway := frame(far)
		now = now.Add(time.Second)
		approachedAgain := frame(near)

		require.Equal(t, []string{"Впереди машина"}, notice)
		require.Equal(t, []string{"Осторожно, впереди близко машина"}, approached)
		require.Empty(t, stillNear)
		require.Empty(t, movedAway)
		require.Equal(t, []string{"Осторожно, впереди близко машина"}, approachedAgain)

		setup(t)
		bicycleNear := object("bicycle", "ahead", "near", 0.9, 0.3)
		bicycleFar := object("bicycle", "ahead", "far", 0.9, 0.05)

		warned := frame(bicycleNear)
		now = now.Add(2 * time.Second)
		notHazard := frame(bicycleFar, object("chair", "left", "near", 0.9, 0.1))
		now = now.Add(2 * time.Second)
		closer := frame(bicycleNear)
		now = now.Add(2 * time.Second)
		gone := frame()
		now = now.Add(2 * time.Second)
		backNear := frame(bicycleNear)

		require.Equal(t, []string{"Осторожно, впереди велосипед"}, warned)
		require.Empty(t, notHazard)
		require.Equal(t, []string{"Осторожно, впереди велосипед"}, closer)
		require.Empty(t, gone)
		require.Empty(t, backNear)
	})

	t.Run("must_pass_prewarm_phrases_contract", func(t *testing.T) {
		setup(t)
		body, err := os.ReadFile(prewarmPhrasesPath)
		require.NoError(t, err)
		contract := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")

		phrases := policy.Phrases()

		require.Equal(t, contract, phrases)
		for _, phrase := range phrases {
			require.False(t, strings.HasSuffix(phrase, "."), phrase)
		}
	})

	t.Run("must_pass_hash", func(t *testing.T) {
		setup(t)
		sum := sha256.Sum256(configs.HazardRules)
		path := writeFile(t, "rules:\n  - name: stairs\n    classes: [stairs]\n    level: danger\n    phrase: Ступени\n")

		custom, err := LoadRules(path)
		fromFile, fromFileErr := LoadRules("../../configs/hazard_rules.yaml")

		require.NoError(t, err)
		require.NoError(t, fromFileErr)
		require.Equal(t, hex.EncodeToString(sum[:])[:12], policy.Hash())
		require.Equal(t, policy.Hash(), fromFile.Hash())
		require.NotEqual(t, policy.Hash(), custom.Hash())
	})

	t.Run("must_pass_custom_file_any_direction_and_distance", func(t *testing.T) {
		setup(t)
		path := writeFile(t, "rules:\n  - name: dog_anywhere\n    classes: [dog]\n    min_confidence: 0.5\n    level: danger\n    phrase: \"Осторожно, {direction} {object}\"\n")
		rules, err := LoadRules(path)
		require.NoError(t, err)

		custom, err := NewPolicy(rules, vocab)

		require.NoError(t, err)
		require.Equal(t, []string{"Осторожно, слева собака", "Осторожно, впереди собака", "Осторожно, справа собака"}, custom.Phrases())
		match, ok := custom.Match(object("dog", "right", "far", 0.5, 0.1))
		require.True(t, ok)
		require.Equal(t, "Осторожно, справа собака", match.Phrase)
		_, ok = custom.Match(object("dog", "right", "far", 0.49, 0.1))
		require.False(t, ok)
	})

	t.Run("must_fail_unknown_class", func(t *testing.T) {
		setup(t)
		rules, err := ParseRules([]byte("rules:\n  - name: stairs_ahead\n    classes: [stairs]\n    level: danger\n    phrase: Ступени\n  - name: wild_animals\n    classes: [dog, unicorn]\n    level: notice\n    phrase: \"Впереди {object}\"\n"))
		require.NoError(t, err)

		custom, err := NewPolicy(rules, vocab)

		require.Nil(t, custom)
		require.EqualError(t, err, `hazard rule "wild_animals": unknown class "unicorn": not in the detector vocabulary`)
	})

	t.Run("must_fail_invalid_yaml", func(t *testing.T) {
		setup(t)

		syntax, syntaxErr := ParseRules([]byte("rules:\n  - name: [stairs\n"))
		unknownField, unknownFieldErr := ParseRules([]byte("rules:\n  - name: stairs\n    class: [stairs]\n    level: danger\n    phrase: Ступени\n"))
		wrongType, wrongTypeErr := ParseRules([]byte("rules:\n  - name: stairs\n    classes: [stairs]\n    min_confidence: high\n    level: danger\n    phrase: Ступени\n"))
		empty, emptyErr := ParseRules([]byte("# no rules\n"))

		require.Nil(t, syntax)
		require.EqualError(t, syntaxErr, "failed to parse hazard rules: yaml: line 1: did not find expected ',' or ']'")
		require.Nil(t, unknownField)
		require.EqualError(t, unknownFieldErr, "failed to parse hazard rules: yaml: unmarshal errors:\n  line 3: field class not found in type hazard.Rule")
		require.Nil(t, wrongType)
		require.EqualError(t, wrongTypeErr, "failed to parse hazard rules: yaml: unmarshal errors:\n  line 4: cannot unmarshal !!str `high` into float64")
		require.Nil(t, empty)
		require.EqualError(t, emptyErr, "failed to parse hazard rules: no rules")
	})

	t.Run("must_fail_invalid_rule", func(t *testing.T) {
		setup(t)
		valid := "    classes: [stairs]\n    level: danger\n    phrase: Ступени\n"
		cases := []struct {
			name     string
			yaml     string
			expected string
		}{
			{"no_name", "rules:\n  - name: \"\"\n" + valid, "hazard rule 1: name is required"},
			{"duplicate_name", "rules:\n  - name: stairs\n" + valid + "  - name: stairs\n" + valid, `hazard rule "stairs": duplicate name`},
			{"no_classes", "rules:\n  - name: stairs\n    level: danger\n    phrase: Ступени\n", `hazard rule "stairs": classes are required`},
			{"direction", "rules:\n  - name: stairs\n    directions: [up]\n" + valid, `hazard rule "stairs": unknown direction "up": allowed values are left, ahead, right`},
			{"distance", "rules:\n  - name: stairs\n    distances: [close]\n" + valid, `hazard rule "stairs": unknown distance "close": allowed values are near, far`},
			{"confidence", "rules:\n  - name: stairs\n    min_confidence: 1.5\n" + valid, `hazard rule "stairs": min_confidence 1.5 is out of [0, 1]`},
			{"level", "rules:\n  - name: stairs\n    classes: [stairs]\n    level: warning\n    phrase: Ступени\n", `hazard rule "stairs": unknown level "warning": allowed values are danger, notice`},
			{"phrase", "rules:\n  - name: stairs\n    classes: [stairs]\n    level: danger\n", `hazard rule "stairs": phrase is required`},
			{"placeholder", "rules:\n  - name: stairs\n    classes: [stairs]\n    level: danger\n    phrase: \"Осторожно, {distance} {object}\"\n", `hazard rule "stairs": unknown placeholder {distance} in phrase: allowed are {direction} and {object}`},
		}

		for _, tc := range cases {
			t.Run(fmt.Sprintf("must_fail_%s", tc.name), func(t *testing.T) {
				rules, err := ParseRules([]byte(tc.yaml))

				require.Nil(t, rules)
				require.EqualError(t, err, tc.expected)
			})
		}
	})

	t.Run("must_fail_file_not_found", func(t *testing.T) {
		setup(t)
		path := filepath.Join(dir, "missing.yaml")

		rules, err := LoadRules(path)

		require.Nil(t, rules)
		require.EqualError(t, err, fmt.Sprintf("failed to read hazard rules file: open %s: no such file or directory", path))
	})
}
