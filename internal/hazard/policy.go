package hazard

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// directions are the directions in the order of the phrases: left, ahead, right.
var directions = []detector.Direction{detector.DirectionLeft, detector.DirectionAhead, detector.DirectionRight}

// directionWords are the words of the directions in the phrases (DSC-3).
var directionWords = map[detector.Direction]string{
	detector.DirectionLeft:  "слева",
	detector.DirectionAhead: "впереди",
	detector.DirectionRight: "справа",
}

// Match is an object of the scene that a rule holds for.
type Match struct {
	Object detector.Object
	Rule   string // name of the rule
	Level  Level
	Phrase string // phrase of the warning, without a final period: it goes to TTS as is
}

// Policy applies the rules checked against the vocabulary. It has no state and is shared by all sessions.
type Policy struct {
	rules   []Rule
	objects map[string]string // class -> name of the class in the phrases (ru.one)
	hash    string
}

// NewPolicy checks every class of the rules against the vocabulary of the detector (PLT-16).
// An unknown class is an error with the name of the rule.
func NewPolicy(rules *Rules, vocabulary *vocabulary.Vocabulary) (*Policy, error) {
	p := &Policy{
		rules:   rules.Rules(),
		objects: map[string]string{},
		hash:    rules.Hash(),
	}
	for _, rule := range p.rules {
		for _, id := range rule.Classes {
			class, ok := vocabulary.Class(id)
			if !ok {
				return nil, fmt.Errorf(errUnknownClassFormat, rule.Name, id)
			}
			p.objects[id] = class.RU.One
		}
	}
	return p, nil
}

// Hash returns the hash of the rules file.
func (p *Policy) Hash() string {
	return p.hash
}

// Match returns the rule the object falls under: the first rule of level danger,
// otherwise the first rule of level notice, in the order of the file.
func (p *Policy) Match(object detector.Object) (Match, bool) {
	var notice *Rule
	for i := range p.rules {
		rule := &p.rules[i]
		if !holds(rule, &object) {
			continue
		}
		if rule.Level == LevelDanger {
			return p.match(rule, object), true
		}
		if notice == nil {
			notice = rule
		}
	}
	if notice == nil {
		return Match{}, false
	}
	return p.match(notice, object), true
}

// Evaluate returns the matches of the objects ordered as DSC-2 orders the hazards:
// danger before notice, then near before far, then by decreasing area.
func (p *Policy) Evaluate(objects []detector.Object) []Match {
	matches := []Match{}
	for _, object := range objects {
		if match, ok := p.Match(object); ok {
			matches = append(matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Level != b.Level {
			return a.Level == LevelDanger
		}
		if a.Object.Distance != b.Object.Distance {
			return a.Object.Distance == detector.DistanceNear
		}
		return a.Object.Area > b.Object.Area
	})
	return matches
}

// Phrases returns every phrase the rules can give, without repeats: by rule in the order of the file,
// by class in the order of the rule, by direction in the order left, ahead, right.
// These are the phrases TTS warms its cache with (testdata/contracts/tts-service/prewarm_phrases.txt).
func (p *Policy) Phrases() []string {
	phrases := []string{}
	seen := map[string]bool{}
	for i := range p.rules {
		rule := &p.rules[i]
		for _, class := range rule.Classes {
			for _, direction := range directions {
				if len(rule.Directions) > 0 && !slices.Contains(rule.Directions, direction) {
					continue
				}
				phrase := p.phrase(rule, class, direction)
				if !seen[phrase] {
					seen[phrase] = true
					phrases = append(phrases, phrase)
				}
			}
		}
	}
	return phrases
}

// match builds the match of the object by the rule.
func (p *Policy) match(rule *Rule, object detector.Object) Match {
	return Match{
		Object: object,
		Rule:   rule.Name,
		Level:  rule.Level,
		Phrase: p.phrase(rule, object.Label, object.Direction),
	}
}

// phrase renders the template of the rule for the class and the direction.
func (p *Policy) phrase(rule *Rule, class string, direction detector.Direction) string {
	return strings.NewReplacer(
		placeholderDirection, directionWords[direction],
		placeholderObject, p.objects[class],
	).Replace(rule.Phrase)
}

// holds reports whether the rule holds for the object: class, direction, distance and confidence.
func holds(rule *Rule, object *detector.Object) bool {
	if !slices.Contains(rule.Classes, object.Label) {
		return false
	}
	if len(rule.Directions) > 0 && !slices.Contains(rule.Directions, object.Direction) {
		return false
	}
	if len(rule.Distances) > 0 && !slices.Contains(rule.Distances, object.Distance) {
		return false
	}
	return object.Confidence >= rule.MinConfidence
}
