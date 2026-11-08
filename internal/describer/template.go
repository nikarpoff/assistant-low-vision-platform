package describer

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/textnorm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Limits of the template description (DSC-4).
const (
	maxHazards = 2
	maxObjects = 5
)

// Phrases of the template description (DSC-3, DSC-4).
const (
	emptyScene     = "Ничего примечательного не вижу. Попробуйте повернуть телефон."
	absentFormat   = "%s не вижу."
	yesPrefix      = "Да, "
	sentenceEnd    = "."
	listSeparator  = ", "
	lastSeparator  = " и "
	pairSeparator  = ", "
	countSeparator = ": "
)

// Question words of the questions «где X», «есть ли X», «X есть» (DSC-4).
const (
	wordWhere = "где"
	wordIs    = "есть"
)

// directionOrder is the order of the groups of the template description: ahead, left, right.
var directionOrder = []detector.Direction{detector.DirectionAhead, detector.DirectionLeft, detector.DirectionRight}

// directionWords are the words of the directions (DSC-3).
var directionWords = map[detector.Direction]string{
	detector.DirectionLeft:  "слева",
	detector.DirectionAhead: "впереди",
	detector.DirectionRight: "справа",
}

// distanceWords are the words of the distances (DSC-3).
var distanceWords = map[detector.Distance]string{
	detector.DistanceNear: "близко",
	detector.DistanceFar:  "далеко",
}

// Template builds the deterministic description of a scene without a language model (DSC-4).
type Template struct {
	vocab  *vocabulary.Vocabulary
	policy *hazard.Policy
}

// NewTemplate creates the template describer over the vocabulary and the hazard rules.
func NewTemplate(vocab *vocabulary.Vocabulary, policy *hazard.Policy) *Template {
	return &Template{vocab: vocab, policy: policy}
}

// Describe returns the description of the scene and whether the scene in focus has a hazard.
// A question «где X», «есть ли X» or «X есть» about a class of the vocabulary gets
// the answer about the class; any other question gets the description of the scene.
func (t *Template) Describe(in *Input) (string, bool) {
	sc := prepareScene(in.Objects, in.Focus, t.policy)
	if answer, ok := t.answer(in.Question, &sc); ok {
		return answer, sc.hasHazard()
	}
	return t.describe(&sc), sc.hasHazard()
}

// describe builds the description: up to two hazards, then up to five other objects
// grouped by direction with their counts.
func (t *Template) describe(sc *scene) string {
	sentences := []string{}
	spoken := map[int]bool{}
	for i := range sc.objects {
		if len(spoken) == maxHazards {
			break
		}
		if sc.danger(i) {
			spoken[i] = true
			sentences = append(sentences, capitalize(sc.matches[i].Phrase)+sentenceEnd)
		}
	}

	rest := []detector.Object{}
	for i, object := range sc.objects {
		if !spoken[i] && len(rest) < maxObjects {
			rest = append(rest, object)
		}
	}
	if len(rest) > 0 {
		sentences = append(sentences, t.groups(rest))
	}

	if len(sentences) == 0 {
		return emptyScene
	}
	return strings.Join(sentences, " ")
}

// groups builds the sentence of the objects grouped by direction in the order ahead, left,
// right: «Впереди два стула и стол, слева шкаф.» Classes keep the order of the objects.
func (t *Template) groups(objects []detector.Object) string {
	groups := []string{}
	for _, direction := range directionOrder {
		classes := []string{}
		counts := map[string]int{}
		for _, object := range objects {
			if object.Direction != direction {
				continue
			}
			if counts[object.Label] == 0 {
				classes = append(classes, object.Label)
			}
			counts[object.Label]++
		}
		if len(classes) == 0 {
			continue
		}
		names := make([]string, len(classes))
		for i, id := range classes {
			names[i] = countPhrase(counts[id], t.forms(id, objects))
		}
		groups = append(groups, directionWords[direction]+" "+joinList(names))
	}
	return capitalize(strings.Join(groups, listSeparator)) + sentenceEnd
}

// answer answers a question «где X», «есть ли X» or «X есть» about a class of the vocabulary.
// ok is false for any other question: it gets the description of the scene.
func (t *Template) answer(question string, sc *scene) (string, bool) {
	normalized := textnorm.Normalize(question)
	words := strings.Fields(normalized)
	where := slices.Contains(words, wordWhere)
	is := slices.Contains(words, wordIs)
	if !where && !is {
		return "", false
	}
	mentions := t.vocab.Mentions(normalized)
	if len(mentions) == 0 {
		return "", false
	}
	id := mentions[0]
	forms := t.forms(id, sc.objects)

	found := []detector.Object{}
	for _, object := range sc.objects {
		if object.Label == id {
			found = append(found, object)
		}
	}
	if len(found) == 0 {
		return capitalize(fmt.Sprintf(absentFormat, absentForm(forms))), true
	}

	// Distinct places of the objects in the order of the scene: the nearest first.
	places := []string{}
	for _, object := range found {
		place := directionWords[object.Direction] + pairSeparator + distanceWords[object.Distance]
		if len(found) > 1 {
			place = directionWords[object.Direction] + " " + distanceWords[object.Distance]
		}
		if !slices.Contains(places, place) {
			places = append(places, place)
		}
	}

	var text string
	if len(found) == 1 {
		text = forms.One + " " + places[0]
	} else {
		text = countPhrase(len(found), forms) + countSeparator + joinList(places)
	}
	if is {
		return yesPrefix + text + sentenceEnd, true
	}
	return capitalize(text) + sentenceEnd, true
}

// forms returns the forms of the class name from the vocabulary; a class the vocabulary
// does not know takes label_ru of the detector for every form.
func (t *Template) forms(id string, objects []detector.Object) vocabulary.Forms {
	if class, ok := t.vocab.Class(id); ok {
		return class.RU
	}
	name := id
	for _, object := range objects {
		if object.Label == id && object.LabelRU != "" {
			name = object.LabelRU
			break
		}
	}
	return vocabulary.Forms{One: name, Few: name, Many: name, Gender: vocabulary.GenderMasculine}
}

// joinList joins the items as a Russian enumeration: «a», «a и b», «a, b и c».
func joinList(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], listSeparator) + lastSeparator + items[len(items)-1]
}

// capitalize makes the first letter of the text uppercase.
func capitalize(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if first == utf8.RuneError {
		return text
	}
	return string(unicode.ToUpper(first)) + text[size:]
}
