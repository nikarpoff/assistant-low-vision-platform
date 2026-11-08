package pipeline

import (
	"strings"
	"unicode"
)

// Reasoning block tags removed from the text of the language model (DSC-7).
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// markupRunes are the markup characters removed from the text (DSC-7).
const markupRunes = "*_#`"

// bulletRunes are the list markers removed at the start of a line (DSC-7).
const bulletRunes = "-•–—"

// Cleanup removes from the text of the describer what is not speech (DSC-7): reasoning
// blocks <think>...</think>, markup characters, list markers and emoji; whitespace
// runs become one space and the leading space is dropped.
//
// The text may be an unfinished stream: an unclosed reasoning block hides the rest of
// the text, and a trailing space is kept, because it tells that the sentence before it
// is over (PLT-7). The cleaned prefix of a stream does not change when more text comes.
func Cleanup(text string) string {
	text = removeThinking(text)

	var builder strings.Builder
	builder.Grow(len(text))
	lineStart := true // no visible character since the start of the text or a line break
	pendingSpace := false
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\n' || r == '\r':
			lineStart = true
			pendingSpace = true
			continue
		case unicode.IsSpace(r):
			pendingSpace = true
			continue
		case strings.ContainsRune(markupRunes, r) || isEmoji(r):
			continue
		}

		if lineStart {
			lineStart = false
			if skip := listMarker(runes[i:]); skip > 0 {
				i += skip - 1
				pendingSpace = true
				continue
			}
		}

		if pendingSpace && builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		pendingSpace = false
		builder.WriteRune(r)
	}
	if pendingSpace && builder.Len() > 0 {
		builder.WriteByte(' ')
	}

	return builder.String()
}

// removeThinking drops the closed reasoning blocks and everything after an unclosed one.
func removeThinking(text string) string {
	for {
		start := strings.Index(text, thinkOpen)
		if start < 0 {
			return text
		}
		end := strings.Index(text[start:], thinkClose)
		if end < 0 {
			return text[:start]
		}
		text = text[:start] + " " + text[start+end+len(thinkClose):]
	}
}

// listMarker returns the number of runes of the list marker at the start of the line:
// a bullet ("- ", "• ") or a number ("1. ", "2) ") followed by a space; 0 if there is none.
// A marker at the end of the unfinished text is not removed yet.
func listMarker(runes []rune) int {
	n := 0
	switch {
	case strings.ContainsRune(bulletRunes, runes[0]):
		n = 1
	case unicode.IsDigit(runes[0]):
		for n < len(runes) && unicode.IsDigit(runes[n]) {
			n++
		}
		if n >= len(runes) || (runes[n] != '.' && runes[n] != ')') {
			return 0
		}
		n++
	default:
		return 0
	}
	if n >= len(runes) || !unicode.IsSpace(runes[n]) {
		return 0
	}
	return n
}

// isEmoji reports whether the rune is an emoji, a pictograph or an emoji modifier.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // pictographs, emoticons, transport, flags, symbols
		return true
	case r >= 0x2600 && r <= 0x27BF: // miscellaneous symbols and dingbats
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // arrows and symbols: ⭐, ⬆
		return true
	case r >= 0x2300 && r <= 0x23FF: // technical: ⌚, ⏰
		return true
	case r == 0x200D || r == 0xFE0F || r == 0x20E3: // joiner, variation selector, keycap
		return true
	case r >= 0xE0020 && r <= 0xE007F: // tags of flag sequences
		return true
	}
	return false
}
