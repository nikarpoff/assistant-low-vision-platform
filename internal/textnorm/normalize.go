// Package textnorm normalizes recognized and answer texts by PLT-4 and ASR-4.
//
// The rule is shared with the ASR service and fixed by the table
// testdata/contracts/asr-service/normalize.cases.json, so the gateway and the
// service give the same text for the same input.
package textnorm

import (
	"strings"
	"unicode"
)

// Normalize returns the text in lowercase, with «ё» replaced by «е», every
// punctuation character (Unicode categories P*) replaced by a space and the
// spaces collapsed to single ones without leading and trailing spaces.
// Symbols (categories S*: «№», «+») and digits are kept; a no-break space is a space.
func Normalize(text string) string {
	text = strings.ToLower(text)
	text = strings.ReplaceAll(text, "ё", "е")
	text = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return ' '
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}
