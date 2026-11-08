package describer

import (
	"strings"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// maxNumeral is the largest count numeralWords spells; larger counts are spelled as «много».
const maxNumeral = 999

// manyWord replaces a count larger than maxNumeral.
const manyWord = "много"

var (
	unitsMasculine = []string{"ноль", "один", "два", "три", "четыре", "пять", "шесть", "семь", "восемь", "девять"}
	teens          = []string{"десять", "одиннадцать", "двенадцать", "тринадцать", "четырнадцать", "пятнадцать", "шестнадцать", "семнадцать", "восемнадцать", "девятнадцать"}
	tens           = []string{"", "", "двадцать", "тридцать", "сорок", "пятьдесят", "шестьдесят", "семьдесят", "восемьдесят", "девяносто"}
	hundreds       = []string{"", "сто", "двести", "триста", "четыреста", "пятьсот", "шестьсот", "семьсот", "восемьсот", "девятьсот"}
)

// numeralWords spells a count in Russian words agreeing with the gender of the noun:
// «один стул», «одна дверь», «одно окно», «две двери». A plurale tantum class
// («ступени» from «ступень») agrees as feminine. Numbers are never given as digits (DSC-3).
func numeralWords(count int, gender vocabulary.Gender) string {
	if count < 0 || count > maxNumeral {
		return manyWord
	}
	if count == 0 {
		return unitsMasculine[0]
	}

	words := []string{}
	if h := count / 100; h > 0 {
		words = append(words, hundreds[h])
	}
	rest := count % 100
	switch {
	case rest >= 10 && rest < 20:
		words = append(words, teens[rest-10])
	default:
		if t := rest / 10; t > 0 {
			words = append(words, tens[t])
		}
		if u := rest % 10; u > 0 {
			words = append(words, unitWord(u, gender))
		}
	}
	return strings.Join(words, " ")
}

// unitWord spells the last digit of a count; 1 and 2 agree with the gender.
func unitWord(unit int, gender vocabulary.Gender) string {
	switch {
	case unit == 1 && (gender == vocabulary.GenderFeminine || gender == vocabulary.GenderPlural):
		return "одна"
	case unit == 1 && gender == vocabulary.GenderNeuter:
		return "одно"
	case unit == 2 && (gender == vocabulary.GenderFeminine || gender == vocabulary.GenderPlural):
		return "две"
	default:
		return unitsMasculine[unit]
	}
}

// countPhrase is a count with the noun: «стул» for one object, «два стула», «пять стульев» for more.
func countPhrase(count int, forms vocabulary.Forms) string {
	if count == 1 {
		return forms.One
	}
	return numeralWords(count, forms.Gender) + " " + forms.ForCount(count)
}

// absentForm is the form of the noun after «не вижу»: the genitive singular, which for
// the vocabulary classes is the form «few» («двери», «стула»), and for a plurale tantum
// the genitive plural, the form «many» («ступеней»).
func absentForm(forms vocabulary.Forms) string {
	if forms.Gender == vocabulary.GenderPlural {
		return forms.Many
	}
	return forms.Few
}
