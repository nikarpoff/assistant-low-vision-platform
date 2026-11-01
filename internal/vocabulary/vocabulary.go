// Package vocabulary is the vocabulary of the detector classes (DET-2, PLT-16):
// the Russian names and forms for the template answers, the class ids for the
// hazard rules and the mention patterns for the scoring of answers.
package vocabulary

import (
	"errors"
	"fmt"
	"regexp"
)

// Texts of the vocabulary errors, shared by the code and the tests.
const (
	errNoClasses          = "vocabulary has no classes"
	errInvalidIDFormat    = "invalid class id %q: lowercase latin snake_case is required"
	errDuplicateFormat    = "duplicate class %q"
	errMissingFormsFormat = "class %q: forms one, few and many are required"
	errGenderFormat       = "class %q: unknown gender %q: allowed values are m, f, n, pl"
	errMentionFormat      = "class %q: invalid mention %q: %w"
)

// mentionFormat wraps a mention pattern so that it matches whole words of the normalized text.
// RE2 has no Unicode \b, so word boundaries are spaces and the ends of the text.
const mentionFormat = `(?:^| )(?:%s)(?: |$)`

var classIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Gender is the grammatical gender of a class name: it selects the form of a numeral («два стула», «две двери»).
type Gender string

// Genders.
const (
	GenderMasculine Gender = "m"
	GenderFeminine  Gender = "f"
	GenderNeuter    Gender = "n"
	GenderPlural    Gender = "pl" // plurale tantum: «ступени»
)

// Document is the body of GET /v1/vocabulary of the detector.
type Document struct {
	Hash    string  `json:"hash"`
	Classes []Class `json:"classes"`
}

// Class is one class of the vocabulary.
type Class struct {
	ID       string   `json:"id"`
	Prompts  []string `json:"prompts"`
	RU       Forms    `json:"ru"`
	Key      bool     `json:"key"`
	Mentions []string `json:"mentions"`
}

// Forms are the Russian forms of a class name.
type Forms struct {
	One    string `json:"one"`  // «стул»
	Few    string `json:"few"`  // «стула»
	Many   string `json:"many"` // «стульев»
	Gender Gender `json:"gender"`
}

// Vocabulary is a checked vocabulary with compiled mention patterns.
type Vocabulary struct {
	hash     string
	classes  []Class
	index    map[string]int
	mentions [][]*regexp.Regexp
}

// NewVocabulary checks the document and compiles the mention patterns.
func NewVocabulary(document *Document) (*Vocabulary, error) {
	if len(document.Classes) == 0 {
		return nil, errors.New(errNoClasses)
	}

	v := &Vocabulary{
		hash:     document.Hash,
		classes:  append([]Class(nil), document.Classes...),
		index:    make(map[string]int, len(document.Classes)),
		mentions: make([][]*regexp.Regexp, len(document.Classes)),
	}

	for i, class := range v.classes {
		if err := checkClass(&class); err != nil {
			return nil, err
		}
		if _, ok := v.index[class.ID]; ok {
			return nil, fmt.Errorf(errDuplicateFormat, class.ID)
		}
		v.index[class.ID] = i

		for _, mention := range class.Mentions {
			pattern, err := regexp.Compile(fmt.Sprintf(mentionFormat, mention))
			if err != nil {
				return nil, fmt.Errorf(errMentionFormat, class.ID, mention, err)
			}
			v.mentions[i] = append(v.mentions[i], pattern)
		}
	}

	return v, nil
}

// Hash returns the hash of the vocabulary given by the detector.
func (v *Vocabulary) Hash() string {
	return v.hash
}

// Classes returns the classes in the order of the vocabulary.
func (v *Vocabulary) Classes() []Class {
	return append([]Class(nil), v.classes...)
}

// Class returns the class with the id.
func (v *Vocabulary) Class(id string) (Class, bool) {
	i, ok := v.index[id]
	if !ok {
		return Class{}, false
	}
	return v.classes[i], true
}

// Has reports whether the vocabulary has the class.
func (v *Vocabulary) Has(id string) bool {
	_, ok := v.index[id]
	return ok
}

// Mentioned reports whether the text mentions the class. The text must be normalized by PLT-4:
// lowercase, «ё» → «е», no punctuation, single spaces.
func (v *Vocabulary) Mentioned(id, text string) bool {
	i, ok := v.index[id]
	if !ok {
		return false
	}
	for _, pattern := range v.mentions[i] {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// Mentions returns the ids of the classes the normalized text mentions, in the order of the vocabulary.
func (v *Vocabulary) Mentions(text string) []string {
	ids := []string{}
	for _, class := range v.classes {
		if v.Mentioned(class.ID, text) {
			ids = append(ids, class.ID)
		}
	}
	return ids
}

// checkClass checks the id and the forms of a class.
func checkClass(class *Class) error {
	if !classIDPattern.MatchString(class.ID) {
		return fmt.Errorf(errInvalidIDFormat, class.ID)
	}
	if class.RU.One == "" || class.RU.Few == "" || class.RU.Many == "" {
		return fmt.Errorf(errMissingFormsFormat, class.ID)
	}
	switch class.RU.Gender {
	case GenderMasculine, GenderFeminine, GenderNeuter, GenderPlural:
		return nil
	default:
		return fmt.Errorf(errGenderFormat, class.ID, class.RU.Gender)
	}
}
