// Package hazard applies the hazard rules to the objects of a scene (PLT-6):
// it finds the hazards (level danger) and the notices (level notice), renders
// the phrases of the warnings and suppresses repeated warnings (cooldown).
//
// The rules are read from configs/hazard_rules.yaml or APP_HAZARD_RULES_PATH.
// Every class of a rule is checked against the vocabulary of the detector
// (PLT-16); an unknown class is an error with the name of the rule.
package hazard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/nikarpoff/assistant-low-vision-platform/configs"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Texts of the errors, shared by the code and the tests.
const (
	errReadFormat          = "failed to read hazard rules file: %w"
	errParseFormat         = "failed to parse hazard rules: %w"
	errNoRules             = "failed to parse hazard rules: no rules"
	errNoNameFormat        = "hazard rule %d: name is required"
	errDuplicateNameFormat = "hazard rule %q: duplicate name"
	errNoClassesFormat     = "hazard rule %q: classes are required"
	errDirectionFormat     = "hazard rule %q: unknown direction %q: allowed values are left, ahead, right"
	errDistanceFormat      = "hazard rule %q: unknown distance %q: allowed values are near, far"
	errConfidenceFormat    = "hazard rule %q: min_confidence %v is out of [0, 1]"
	errLevelFormat         = "hazard rule %q: unknown level %q: allowed values are danger, notice"
	errNoPhraseFormat      = "hazard rule %q: phrase is required"
	errPlaceholderFormat   = "hazard rule %q: unknown placeholder %s in phrase: allowed are {direction} and {object}"
	errUnknownClassFormat  = "hazard rule %q: unknown class %q: not in the detector vocabulary"
)

// hashLength is the number of hex characters of SHA-256 in the hash of the file.
const hashLength = 12

// Placeholders of the phrase template.
const (
	placeholderDirection = "{direction}"
	placeholderObject    = "{object}"
)

var placeholderPattern = regexp.MustCompile(`\{[^{}]*\}`)

// Level is the level of a rule.
type Level string

// Levels.
const (
	// LevelDanger is a hazard: a warning in the passive mode and hazard=true in the descriptions.
	LevelDanger Level = "danger"
	// LevelNotice is a hint: spoken in the passive mode only with APP_PASSIVE_NOTICES.
	LevelNotice Level = "notice"
)

// Rule is one hazard rule of the file.
type Rule struct {
	Name    string   `yaml:"name"`
	Classes []string `yaml:"classes"`
	// Directions the rule holds for; empty - any direction.
	Directions []detector.Direction `yaml:"directions"`
	// Distances the rule holds for; empty - any distance.
	Distances     []detector.Distance `yaml:"distances"`
	MinConfidence float64             `yaml:"min_confidence"`
	Level         Level               `yaml:"level"`
	// Phrase is the template of the phrase with {direction} and {object}.
	Phrase string `yaml:"phrase"`
}

// Rules are the checked rules of a file and the hash of the file.
type Rules struct {
	rules []Rule
	hash  string
}

// rulesFile is the document of the rules file.
type rulesFile struct {
	Rules []Rule `yaml:"rules"`
}

// LoadRules reads the rules file at path; an empty path gives the embedded configs/hazard_rules.yaml.
func LoadRules(path string) (*Rules, error) {
	if path == "" {
		return ParseRules(configs.HazardRules)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFormat, err)
	}
	return ParseRules(data)
}

// ParseRules parses and checks the YAML of the rules. Unknown fields are errors.
// The classes are checked later against the vocabulary by NewPolicy.
func ParseRules(data []byte) (*Rules, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	document := rulesFile{}
	if err := decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf(errParseFormat, err)
	}
	if len(document.Rules) == 0 {
		return nil, errors.New(errNoRules)
	}

	names := map[string]bool{}
	for i := range document.Rules {
		rule := &document.Rules[i]
		if rule.Name == "" {
			return nil, fmt.Errorf(errNoNameFormat, i+1)
		}
		if names[rule.Name] {
			return nil, fmt.Errorf(errDuplicateNameFormat, rule.Name)
		}
		names[rule.Name] = true
		if err := checkRule(rule); err != nil {
			return nil, err
		}
	}

	return &Rules{rules: document.Rules, hash: fileHash(data)}, nil
}

// Hash returns the first 12 hex characters of SHA-256 of the rules file: it goes into config_id (PLT-12).
func (r *Rules) Hash() string {
	return r.hash
}

// Rules returns the rules in the order of the file.
func (r *Rules) Rules() []Rule {
	return append([]Rule(nil), r.rules...)
}

// checkRule checks the fields of a rule except the classes against the vocabulary.
func checkRule(rule *Rule) error {
	if len(rule.Classes) == 0 {
		return fmt.Errorf(errNoClassesFormat, rule.Name)
	}
	for _, direction := range rule.Directions {
		if _, ok := directionWords[direction]; !ok {
			return fmt.Errorf(errDirectionFormat, rule.Name, direction)
		}
	}
	for _, distance := range rule.Distances {
		if distance != detector.DistanceNear && distance != detector.DistanceFar {
			return fmt.Errorf(errDistanceFormat, rule.Name, distance)
		}
	}
	if rule.MinConfidence < 0 || rule.MinConfidence > 1 {
		return fmt.Errorf(errConfidenceFormat, rule.Name, rule.MinConfidence)
	}
	if rule.Level != LevelDanger && rule.Level != LevelNotice {
		return fmt.Errorf(errLevelFormat, rule.Name, rule.Level)
	}
	if rule.Phrase == "" {
		return fmt.Errorf(errNoPhraseFormat, rule.Name)
	}
	for _, placeholder := range placeholderPattern.FindAllString(rule.Phrase, -1) {
		if placeholder != placeholderDirection && placeholder != placeholderObject {
			return fmt.Errorf(errPlaceholderFormat, rule.Name, placeholder)
		}
	}
	return nil
}

// fileHash returns the first 12 hex characters of SHA-256 of the file.
func fileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:hashLength]
}
