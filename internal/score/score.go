package score

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/loadgen"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/textnorm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// maxLineBytes is the limit of a JSONL line: an answer is at most a few hundred characters.
const maxLineBytes = 1024 * 1024

// sentenceEnds are the characters that end a sentence when a space or the end of the text follows (PLT-7).
const sentenceEnds = ".!?…"

// wordFormat wraps a pattern so that it matches whole words of the normalized text, as the mentions of the vocabulary.
const wordFormat = `(?:^| )(?:%s)(?: |$)`

// Texts of the JSONL errors, shared by the code and the tests.
const (
	errNoRunFormat        = "%q line %d: run is required"
	errDuplicateRunFormat = "run %d is in both %q and %q: every run must have its own number (loadgen --first-run)"
	errUnknownImageFormat = "%q line %d: image %d is not in descriptions.jsonl"
)

// Scorer finds the key objects and the mentions in the answer texts.
type Scorer struct {
	vocabulary *vocabulary.Vocabulary
	images     map[int]*Image
	directions map[detector.Direction]*regexp.Regexp
}

// NewScorer creates a scorer over the vocabulary and the annotation of the data set.
func NewScorer(vocab *vocabulary.Vocabulary, images map[int]*Image) *Scorer {
	directions := make(map[detector.Direction]*regexp.Regexp, len(directionPatterns))
	for direction, pattern := range directionPatterns {
		directions[direction] = regexp.MustCompile(fmt.Sprintf(wordFormat, pattern))
	}
	return &Scorer{vocabulary: vocab, images: images, directions: directions}
}

// Input is the records of the JSONL files of loadgen.
type Input struct {
	Files   []string
	Entries []Entry
}

// Entry is a JSONL line: a request and where it comes from.
type Entry struct {
	Record loadgen.Record
	File   string
	Line   int
}

// ReadRecords reads one or more JSONL files of loadgen. The runs are told apart by the field
// run; a run number found in two files is an error.
func ReadRecords(paths []string) (*Input, error) {
	input := &Input{Files: append([]string{}, paths...)}
	owners := map[int]string{} // run -> file
	for _, path := range paths {
		entries, err := readFile(path)
		if err != nil {
			return nil, err
		}
		runs := map[int]bool{}
		for i := range entries {
			run := entries[i].Record.Run
			if owner, ok := owners[run]; ok && owner != path {
				return nil, fmt.Errorf(errDuplicateRunFormat, run, owner, path)
			}
			runs[run] = true
		}
		for run := range runs {
			owners[run] = path
		}
		input.Entries = append(input.Entries, entries...)
	}
	return input, nil
}

// readFile reads the lines of a JSONL file.
func readFile(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	defer file.Close()

	entries := []Entry{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var record loadgen.Record
		if err := json.Unmarshal([]byte(text), &record); err != nil {
			return nil, fmt.Errorf(errParseLineFormat, path, line, err)
		}
		if record.Run < 1 {
			return nil, fmt.Errorf(errNoRunFormat, path, line)
		}
		entries = append(entries, Entry{Record: record, File: path, Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	return entries, nil
}

// Score computes the metrics of EXP-4 for every run, for the union of the runs and for every
// snapshot. Only the describe requests (description by touch, mode=on_demand) are scored:
// their answer describes the whole scene. A describe request without answer.end (error,
// timeout) is counted as unanswered and is not scored.
func (s *Scorer) Score(input *Input) (*Result, error) {
	result := &Result{
		ConfigIDs: []string{},
		Files:     append([]string{}, input.Files...),
		Runs:      []RunTotals{},
		Images:    []ImageTotals{},
	}

	byRun := map[int]*Totals{}
	byImage := map[int]*Totals{}
	pooled := &Totals{}
	for i := range input.Entries {
		entry := &input.Entries[i]
		record := &entry.Record
		if record.ConfigID != "" && !slices.Contains(result.ConfigIDs, record.ConfigID) {
			result.ConfigIDs = append(result.ConfigIDs, record.ConfigID)
		}
		if byRun[record.Run] == nil {
			byRun[record.Run] = &Totals{}
		}
		if record.Kind != loadgen.ScenarioDescribe || record.ImageID == nil {
			continue
		}

		image, ok := s.images[*record.ImageID]
		if !ok {
			return nil, fmt.Errorf(errUnknownImageFormat, entry.File, entry.Line, *record.ImageID)
		}
		if byImage[image.ID] == nil {
			byImage[image.ID] = &Totals{}
		}

		var answer Totals
		if record.Answered() {
			answer = s.ScoreAnswer(record.Text, image)
		} else {
			answer.Unanswered = 1
		}
		for _, totals := range []*Totals{byRun[record.Run], byImage[image.ID], pooled} {
			totals.add(&answer)
		}
	}

	runs := make([]int, 0, len(byRun))
	for run := range byRun {
		runs = append(runs, run)
	}
	sort.Ints(runs)
	for _, run := range runs {
		byRun[run].shares()
		result.Runs = append(result.Runs, RunTotals{Run: run, Totals: *byRun[run]})
	}

	pooled.shares()
	result.Pooled = *pooled
	result.Spread = spread(result.Runs)

	ids := make([]int, 0, len(byImage))
	for id := range byImage {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		byImage[id].shares()
		result.Images = append(result.Images, ImageTotals{ImageID: id, Scene: s.images[id].Scene, Totals: *byImage[id]})
	}
	return result, nil
}

// ScoreAnswer scores one answer text against the annotation of its snapshot (EXP-4):
//   - a key object is named with its class when the text mentions the class;
//   - it is named with the direction when a sentence mentions the class and the right direction;
//   - a mention is a sentence mentioning a key class (key: true in the vocabulary); it is
//     hallucinated when the class is absent from the annotation of the snapshot.
func (s *Scorer) ScoreAnswer(text string, image *Image) Totals {
	sentences := []string{}
	for _, sentence := range splitSentences(text) {
		if normalized := textnorm.Normalize(sentence); normalized != "" {
			sentences = append(sentences, normalized)
		}
	}
	whole := strings.Join(sentences, " ")

	totals := Totals{Answers: 1, KeyObjects: len(image.KeyObjects)}
	for _, object := range image.KeyObjects {
		if s.vocabulary.Mentioned(object.Class, whole) {
			totals.NamedClassOnly++
		}
		for _, sentence := range sentences {
			if s.vocabulary.Mentioned(object.Class, sentence) && s.directions[object.Direction].MatchString(sentence) {
				totals.NamedWithDirection++
				break
			}
		}
	}

	for _, sentence := range sentences {
		for _, id := range s.vocabulary.Mentions(sentence) {
			class, _ := s.vocabulary.Class(id)
			if !class.Key {
				continue
			}
			totals.Mentions++
			if !image.Classes[id] {
				totals.HallucinatedMentions++
			}
		}
	}

	totals.shares()
	return totals
}

// splitSentences cuts the text after ".", "!", "?" or "…" followed by a space or the end of the text.
func splitSentences(text string) []string {
	sentences := []string{}
	start := 0
	for i, r := range text {
		if !strings.ContainsRune(sentenceEnds, r) {
			continue
		}
		next := i + utf8.RuneLen(r)
		if next < len(text) {
			following, _ := utf8.DecodeRuneInString(text[next:])
			if !unicode.IsSpace(following) {
				continue
			}
		}
		sentences = append(sentences, text[start:next])
		start = next
	}
	if start < len(text) {
		sentences = append(sentences, text[start:])
	}
	return sentences
}

// add adds the counts of other.
func (t *Totals) add(other *Totals) {
	t.Answers += other.Answers
	t.Unanswered += other.Unanswered
	t.KeyObjects += other.KeyObjects
	t.NamedWithDirection += other.NamedWithDirection
	t.NamedClassOnly += other.NamedClassOnly
	t.Mentions += other.Mentions
	t.HallucinatedMentions += other.HallucinatedMentions
}

// shares computes the shares from the counts; a share without its denominator is nil.
func (t *Totals) shares() {
	t.KeyObjectsWithDirection = ratio(t.NamedWithDirection, t.KeyObjects)
	t.KeyObjectsClassOnly = ratio(t.NamedClassOnly, t.KeyObjects)
	t.Hallucinated = ratio(t.HallucinatedMentions, t.Mentions)
}

// ratio returns part/total or nil when total is 0.
func ratio(part, total int) *float64 {
	if total == 0 {
		return nil
	}
	value := float64(part) / float64(total)
	return &value
}

// spread computes the minimum, the maximum and the mean of the shares over the runs.
func spread(runs []RunTotals) Spread {
	pick := func(share func(*Totals) *float64) *Range {
		var r *Range
		count := 0
		for i := range runs {
			value := share(&runs[i].Totals)
			if value == nil {
				continue
			}
			if r == nil {
				r = &Range{Min: *value, Max: *value}
			}
			r.Min = min(r.Min, *value)
			r.Max = max(r.Max, *value)
			r.Mean += *value
			count++
		}
		if r != nil {
			r.Mean /= float64(count)
		}
		return r
	}
	return Spread{
		KeyObjectsWithDirection: pick(func(t *Totals) *float64 { return t.KeyObjectsWithDirection }),
		KeyObjectsClassOnly:     pick(func(t *Totals) *float64 { return t.KeyObjectsClassOnly }),
		Hallucinated:            pick(func(t *Totals) *float64 { return t.Hallucinated }),
	}
}
