package vocabulary

// Plural is the Russian plural category of a count.
type Plural string

// Plural categories.
const (
	PluralOne  Plural = "one"  // 1, 21, 101: «стул»
	PluralFew  Plural = "few"  // 2-4, 22-24: «стула»
	PluralMany Plural = "many" // 0, 5-20, 25-30: «стульев»
)

// PluralOf returns the plural category of the count by the Russian rules.
func PluralOf(count int) Plural {
	if count < 0 {
		count = -count
	}
	lastTwo := count % 100
	last := count % 10

	switch {
	case lastTwo >= 11 && lastTwo <= 14:
		return PluralMany
	case last == 1:
		return PluralOne
	case last >= 2 && last <= 4:
		return PluralFew
	default:
		return PluralMany
	}
}

// ForCount returns the form of the name that goes after the count: 1 «стул», 2 «стула», 5 «стульев».
func (f Forms) ForCount(count int) string {
	switch PluralOf(count) {
	case PluralOne:
		return f.One
	case PluralFew:
		return f.Few
	default:
		return f.Many
	}
}
