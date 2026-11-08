package describer

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// forbiddenPattern matches digits, markup characters and list markers at the start of a line (DSC-3).
var forbiddenPattern = regexp.MustCompile("[0-9*_#`\\[\\]<>]|(?m)^\\s*[-•]")

// sentencePattern counts sentences by PLT-7: a terminator followed by a space or the end.
var sentencePattern = regexp.MustCompile(`[.!?…](?:\s|$)`)

func TestTemplateDescriber(t *testing.T) {
	var (
		vocab    *vocabulary.Vocabulary
		policy   *hazard.Policy
		template *Template
	)

	setup := func(t *testing.T) {
		vocab = readVocabulary(t)
		policy = readPolicy(t, vocab)
		template = NewTemplate(vocab, policy)
	}

	describe := func(objects []detector.Object, focus protocol.Focus, question string) (string, bool) {
		return template.Describe(&Input{Objects: objects, Focus: focus, Question: question})
	}

	t.Run("must_pass_hazards_first", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{
			object("table", detector.DirectionLeft, detector.DistanceNear, 0.4),
			object("pole", detector.DirectionAhead, detector.DistanceNear, 0.05),
			object("car", detector.DirectionRight, detector.DistanceNear, 0.2),
			object("stairs", detector.DirectionAhead, detector.DistanceNear, 0.3),
		}

		text, hazardFound := describe(objects, protocol.FocusAll, "")

		require.True(t, hazardFound)
		require.Equal(t, "Осторожно, впереди ступени. Осторожно, справа близко машина. Впереди столб, слева стол.", text)
	})

	t.Run("must_pass_notice_is_not_hazard", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{object("person", detector.DirectionAhead, detector.DistanceNear, 0.2)}

		text, hazardFound := describe(objects, protocol.FocusAll, "")

		require.False(t, hazardFound)
		require.Equal(t, "Впереди человек.", text)
	})

	t.Run("must_pass_groups_and_counts", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{
			object("chair", detector.DirectionAhead, detector.DistanceFar, 0.06),
			object("wardrobe", detector.DirectionLeft, detector.DistanceFar, 0.2),
			object("table", detector.DirectionAhead, detector.DistanceFar, 0.1),
			object("chair", detector.DirectionAhead, detector.DistanceFar, 0.05),
		}

		text, _ := describe(objects, protocol.FocusAll, "")

		require.Equal(t, "Впереди стол и два стула, слева шкаф.", text)
	})

	t.Run("must_pass_count_forms", func(t *testing.T) {
		setup(t)
		repeat := func(label string, direction detector.Direction, count int) []detector.Object {
			objects := []detector.Object{}
			for i := 0; i < count; i++ {
				objects = append(objects, object(label, direction, detector.DistanceFar, 0.1))
			}
			return objects
		}

		chairs, _ := describe(repeat("chair", detector.DirectionAhead, 5), protocol.FocusAll, "")
		doors, _ := describe(repeat("door", detector.DirectionRight, 2), protocol.FocusAll, "")
		windows, _ := describe(repeat("window", detector.DirectionLeft, 2), protocol.FocusAll, "")
		mixed, _ := describe(append(repeat("door", detector.DirectionLeft, 3), repeat("bench", detector.DirectionLeft, 2)...), protocol.FocusAll, "")

		require.Equal(t, "Впереди пять стульев.", chairs)
		require.Equal(t, "Справа две двери.", doors)
		require.Equal(t, "Слева два окна.", windows)
		require.Equal(t, "Слева три двери и две скамейки.", mixed)
	})

	t.Run("must_pass_max_five_objects", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{
			object("bed", detector.DirectionAhead, detector.DistanceNear, 0.30),
			object("sofa", detector.DirectionAhead, detector.DistanceNear, 0.25),
			object("table", detector.DirectionLeft, detector.DistanceNear, 0.20),
			object("tv", detector.DirectionRight, detector.DistanceNear, 0.15),
			object("window", detector.DirectionRight, detector.DistanceFar, 0.40),
			object("sink", detector.DirectionLeft, detector.DistanceFar, 0.10),
			object("toilet", detector.DirectionLeft, detector.DistanceFar, 0.05),
		}

		text, _ := describe(objects, protocol.FocusAll, "")

		require.Equal(t, "Впереди кровать и диван, слева стол, справа телевизор и окно.", text)
		require.NotContains(t, text, "раковина")
		require.NotContains(t, text, "унитаз")
	})

	t.Run("must_pass_empty_scene", func(t *testing.T) {
		setup(t)

		empty, hazardFound := describe(nil, protocol.FocusAll, "")
		nothingAhead, _ := describe([]detector.Object{object("chair", detector.DirectionLeft, detector.DistanceFar, 0.1)}, protocol.FocusAhead, "")

		require.False(t, hazardFound)
		require.Equal(t, "Ничего примечательного не вижу. Попробуйте повернуть телефон.", empty)
		require.Equal(t, empty, nothingAhead)
	})

	t.Run("must_pass_question_where", func(t *testing.T) {
		setup(t)
		objects := append(roomObjects(), object("chair", detector.DirectionAhead, detector.DistanceFar, 0.02))

		door, _ := describe(objects, protocol.FocusAll, "Где дверь?")
		chairs, _ := describe(objects, protocol.FocusAll, "Где стулья?")

		require.Equal(t, "Дверь справа, далеко.", door)
		require.Equal(t, "Два стула: слева близко и впереди далеко.", chairs)
	})

	t.Run("must_pass_question_is", func(t *testing.T) {
		setup(t)
		objects := append(roomObjects(), object("stairs", detector.DirectionAhead, detector.DistanceNear, 0.2))

		isThere, hazardFound := describe(objects, protocol.FocusAll, "Есть ли лестница?")
		thereIs, _ := describe(objects, protocol.FocusAll, "Кровать есть?")

		require.True(t, hazardFound)
		require.Equal(t, "Да, ступени впереди, близко.", isThere)
		require.Equal(t, "Да, кровать впереди, близко.", thereIs)
	})

	t.Run("must_pass_question_absent", func(t *testing.T) {
		setup(t)

		window, _ := describe(roomObjects(), protocol.FocusAll, "Где окно?")
		stairs, _ := describe(roomObjects(), protocol.FocusAll, "Есть ли ступеньки?")
		car, _ := describe(roomObjects(), protocol.FocusAll, "Машина есть?")

		require.Equal(t, "Окна не вижу.", window)
		require.Equal(t, "Ступеней не вижу.", stairs)
		require.Equal(t, "Машины не вижу.", car)
	})

	t.Run("must_pass_other_question_describes_scene", func(t *testing.T) {
		setup(t)

		other, _ := describe(roomObjects(), protocol.FocusAll, "Что это за комната?")
		unknownClass, _ := describe(roomObjects(), protocol.FocusAll, "Где мой рюкзак?")

		require.Equal(t, "Впереди кровать, слева стул и шкаф, справа дверь.", other)
		require.Equal(t, other, unknownClass)
	})

	t.Run("must_pass_dsc8_dialog", func(t *testing.T) {
		setup(t)

		description, _ := describe(roomObjects(), protocol.FocusAll, "")
		people, _ := describe(roomObjects(), protocol.FocusAll, "В комнате есть люди?")
		door, _ := describe(roomObjects(), protocol.FocusAll, "Где-то есть дверь?")

		require.Equal(t, "Впереди кровать, слева стул и шкаф, справа дверь.", description)
		require.Equal(t, "Человека не вижу.", people)
		require.Equal(t, "Да, дверь справа, далеко.", door)
	})

	t.Run("must_pass_no_digits_and_markup", func(t *testing.T) {
		setup(t)
		scenes := [][]detector.Object{nil, roomObjects()}
		for _, class := range vocab.Classes() {
			for _, direction := range directionOrder {
				for _, distance := range []detector.Distance{detector.DistanceNear, detector.DistanceFar} {
					scenes = append(scenes, []detector.Object{
						object(class.ID, direction, distance, 0.2),
						object(class.ID, direction, distance, 0.1),
						object("stairs", detector.DirectionAhead, detector.DistanceNear, 0.3),
						object("car", detector.DirectionLeft, detector.DistanceNear, 0.25),
						object("chair", detector.DirectionRight, detector.DistanceFar, 0.05),
					})
				}
			}
		}
		questions := []string{"", "Где дверь?", "Есть ли люди?", "Стулья есть?", "Что вокруг?"}

		for _, objects := range scenes {
			for _, question := range questions {
				text, _ := describe(objects, protocol.FocusAll, question)
				require.NotRegexp(t, forbiddenPattern, text)
				require.LessOrEqual(t, utf8.RuneCountInString(text), 220, text)
				require.LessOrEqual(t, len(sentencePattern.FindAllString(text, -1)), 3, text)
				require.True(t, strings.HasSuffix(text, "."), text)
				for _, word := range []string{"сверху", "снизу", "сзади", "метр"} {
					require.NotContains(t, text, word)
				}
			}
		}
	})

	t.Run("must_pass_focus_ahead", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{
			object("car", detector.DirectionLeft, detector.DistanceNear, 0.3),
			object("table", detector.DirectionAhead, detector.DistanceFar, 0.2),
			object("door", detector.DirectionRight, detector.DistanceFar, 0.1),
			object("chair", detector.DirectionAhead, detector.DistanceNear, 0.05),
		}

		text, hazardFound := describe(objects, protocol.FocusAhead, "")
		door, _ := describe(objects, protocol.FocusAhead, "Где дверь?")

		require.False(t, hazardFound)
		require.Equal(t, "Впереди стул и стол.", text)
		require.Equal(t, "Двери не вижу.", door)
	})

	t.Run("must_pass_unknown_class", func(t *testing.T) {
		setup(t)
		objects := []detector.Object{{Label: "rocket", LabelRU: "ракета", Direction: detector.DirectionAhead, Distance: detector.DistanceFar, Area: 0.1}}

		text, _ := describe(objects, protocol.FocusAll, "")

		require.Equal(t, "Впереди ракета.", text)
	})
}

func TestNumerals(t *testing.T) {
	t.Run("must_pass_words", func(t *testing.T) {
		cases := []struct {
			count  int
			gender vocabulary.Gender
			want   string
		}{
			{0, vocabulary.GenderMasculine, "ноль"},
			{1, vocabulary.GenderMasculine, "один"},
			{1, vocabulary.GenderFeminine, "одна"},
			{1, vocabulary.GenderNeuter, "одно"},
			{2, vocabulary.GenderMasculine, "два"},
			{2, vocabulary.GenderNeuter, "два"},
			{2, vocabulary.GenderFeminine, "две"},
			{2, vocabulary.GenderPlural, "две"},
			{5, vocabulary.GenderMasculine, "пять"},
			{11, vocabulary.GenderFeminine, "одиннадцать"},
			{20, vocabulary.GenderMasculine, "двадцать"},
			{21, vocabulary.GenderFeminine, "двадцать одна"},
			{42, vocabulary.GenderFeminine, "сорок две"},
			{100, vocabulary.GenderMasculine, "сто"},
			{312, vocabulary.GenderMasculine, "триста двенадцать"},
			{999, vocabulary.GenderMasculine, "девятьсот девяносто девять"},
			{1000, vocabulary.GenderMasculine, "много"},
			{-1, vocabulary.GenderMasculine, "много"},
		}
		for _, c := range cases {
			require.Equal(t, c.want, numeralWords(c.count, c.gender), c.count)
		}
	})

	t.Run("must_pass_count_phrase", func(t *testing.T) {
		chair := vocabulary.Forms{One: "стул", Few: "стула", Many: "стульев", Gender: vocabulary.GenderMasculine}
		door := vocabulary.Forms{One: "дверь", Few: "двери", Many: "дверей", Gender: vocabulary.GenderFeminine}

		require.Equal(t, "стул", countPhrase(1, chair))
		require.Equal(t, "два стула", countPhrase(2, chair))
		require.Equal(t, "пять стульев", countPhrase(5, chair))
		require.Equal(t, "двадцать один стул", countPhrase(21, chair))
		require.Equal(t, "две двери", countPhrase(2, door))
		require.Equal(t, "двери", absentForm(door))
		require.Equal(t, "стула", absentForm(chair))
	})
}
