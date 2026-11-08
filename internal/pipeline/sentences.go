package pipeline

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of the sentence stream (PLT-7).
const (
	// MaxSentences is the number of sentences in an answer.
	MaxSentences = 3
	// MinSentenceChars is the length below which a sentence is joined with the next one.
	MinSentenceChars = 12
)

// sentenceEnds are the characters that end a sentence when a space or the end of the stream follows (PLT-7).
const sentenceEnds = ".!?…"

// Sentences cuts the text stream of the describer into sentences (PLT-7).
//
// The text is cleaned by Cleanup (DSC-7) and cut after ".", "!", "?" or "…" followed by
// a space or the end of the stream. A sentence shorter than MinSentenceChars is joined
// with the next one, except the last. The answer holds at most MaxSentences sentences
// and maxChars characters: once a limit is reached, Push reports that the stream is to be
// closed and the rest of the text is dropped.
type Sentences struct {
	maxChars int
	raw      strings.Builder
	offset   int    // bytes of the cleaned text already cut into sentences
	carry    string // a short sentence waiting for the next one
	count    int    // sentences given out
	chars    int    // characters of the answer given out, with the spaces between sentences
	done     bool
}

// NewSentences creates the splitter with the limit of characters APP_MAX_ANSWER_CHARS.
func NewSentences(maxChars int) *Sentences {
	return &Sentences{maxChars: maxChars}
}

// Push adds a piece of the stream and returns the sentences it completed.
// done is true when a limit is reached: the stream is to be closed.
func (s *Sentences) Push(text string) (sentences []string, done bool) {
	if s.done {
		return nil, true
	}
	s.raw.WriteString(text)
	clean := Cleanup(s.raw.String())
	s.offset = min(s.offset, len(clean))

	for !s.done {
		end := sentenceEnd(clean, s.offset)
		if end < 0 {
			break
		}
		sentence := strings.TrimSpace(clean[s.offset:end])
		s.offset = end
		sentences = s.add(sentences, sentence)
	}

	// The unfinished sentence cannot fit any more: stop the stream now. The first sentence
	// of the answer is cut at a word instead.
	if !s.done {
		pending := strings.TrimSpace(clean[s.offset:])
		if s.carry != "" {
			pending = strings.TrimSpace(s.carry + " " + pending)
		}
		need := utf8.RuneCountInString(pending)
		if s.count > 0 {
			need++
		}
		if pending != "" && s.chars+need > s.maxChars {
			if s.count == 0 {
				s.carry = ""
				sentences = s.emit(sentences, pending)
			} else {
				sentences = s.flushCarry(sentences)
			}
			s.done = true
		}
	}

	return sentences, s.done
}

// Flush ends the stream and returns the rest of the text as the last sentence.
func (s *Sentences) Flush() []string {
	if s.done {
		return nil
	}
	clean := Cleanup(s.raw.String())
	var sentences []string
	if rest := strings.TrimSpace(clean[min(s.offset, len(clean)):]); rest != "" {
		sentences = s.add(sentences, rest)
	}
	sentences = s.flushCarry(sentences)
	s.done = true
	return sentences
}

// add joins the sentence with a waiting short one and gives it out unless it is still short.
func (s *Sentences) add(sentences []string, sentence string) []string {
	if sentence == "" {
		return sentences
	}
	if s.carry != "" {
		joined := s.carry + " " + sentence
		if !s.fits(joined) {
			// The joined sentence is too long: the short one goes alone and ends the answer.
			sentences = s.flushCarry(sentences)
			s.done = true
			return sentences
		}
		sentence = joined
		s.carry = ""
	}
	if utf8.RuneCountInString(sentence) < MinSentenceChars {
		s.carry = sentence
		return sentences
	}
	return s.emit(sentences, sentence)
}

// flushCarry gives out the waiting short sentence, if any.
func (s *Sentences) flushCarry(sentences []string) []string {
	if s.carry == "" {
		return sentences
	}
	carry := s.carry
	s.carry = ""
	return s.emit(sentences, carry)
}

// emit gives out the sentence within the limits. The first sentence of the answer
// longer than the character limit is cut at a word; any other one that does not fit
// ends the answer.
func (s *Sentences) emit(sentences []string, sentence string) []string {
	if !s.fits(sentence) {
		if s.count == 0 {
			sentences = append(sentences, truncateWords(sentence, s.maxChars))
			s.count++
		}
		s.done = true
		return sentences
	}

	if s.count > 0 {
		s.chars++ // the space between sentences
	}
	s.chars += utf8.RuneCountInString(sentence)
	s.count++
	sentences = append(sentences, sentence)
	if s.count >= MaxSentences || s.chars >= s.maxChars {
		s.done = true
	}
	return sentences
}

// fits reports whether the sentence fits into the characters left.
func (s *Sentences) fits(sentence string) bool {
	chars := s.chars + utf8.RuneCountInString(sentence)
	if s.count > 0 {
		chars++
	}
	return chars <= s.maxChars
}

// sentenceEnd returns the byte offset after the end of the first complete sentence that
// starts at from: after the last of the ending characters that a space follows; -1 if
// there is no complete sentence yet. The end of the stream is handled by Flush.
func sentenceEnd(text string, from int) int {
	for i, r := range text[from:] {
		if !strings.ContainsRune(sentenceEnds, r) {
			continue
		}
		next := from + i + utf8.RuneLen(r)
		if next < len(text) {
			following, _ := utf8.DecodeRuneInString(text[next:])
			if unicode.IsSpace(following) {
				return next
			}
		}
	}
	return -1
}

// truncateWords cuts the text to at most limit characters at the last space.
func truncateWords(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := string(runes[:limit])
	if space := strings.LastIndexByte(cut, ' '); space > 0 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:-")
}
