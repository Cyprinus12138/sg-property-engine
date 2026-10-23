package httpapi

import (
	"strings"
	"unicode"
)

// URA's PMI / transactions / pipeline endpoints emit project names,
// streets, and other text in ALL-CAPS ("COASTAL CABANA", "BEDOK SOUTH
// AVENUE 1"). The DB stores them as-is so the case-insensitive join
// keys remain stable; serializers convert to a display-friendly Title
// Case at the boundary.
//
// SG-specific concerns the helper handles:
//   - Acronyms / building-type suffixes that must stay uppercase:
//     EC, HDB, MRT, BTO, GLS, …
//   - Roman numerals on landed sub-districts and en-bloc names: II,
//     III, IV, V, VI, … XV.
//   - Apostrophes in proper nouns: D'Leedon, J'Den — cap the letter
//     after the apostrophe, not just the first letter.
//   - Hyphen-joined words: cap each segment.
//
// Strings that are not all-uppercase are returned unchanged, so this
// is safe to call on third-party data that is already
// titlecased.

// keepUpper is a closed set of tokens that must stay uppercase even
// after titlecasing. Append to this list as new SG abbreviations
// surface in the wild — the helper is intentionally conservative.
var keepUpper = map[string]struct{}{
	// Property/building suffixes
	"EC": {}, "HDB": {}, "EA": {}, "DBSS": {}, "BTO": {}, "BTLE": {},
	// Transit
	"MRT": {}, "LRT": {},
	// Status / lifecycle
	"GLS": {}, "TBA": {}, "TBC": {}, "TOP": {},
	// Agencies (rarely in names but sometimes)
	"URA": {}, "BCA": {}, "LTA": {}, "PSA": {}, "SLA": {}, "JTC": {},
	// Roman numerals — landed sub-districts (e.g. "VILLA DELLE ROSE II")
	"II": {}, "III": {}, "IV": {}, "V": {}, "VI": {},
	"VII": {}, "VIII": {}, "IX": {}, "X": {}, "XI": {},
	"XII": {}, "XIII": {}, "XIV": {}, "XV": {},
}

// titlecaseURA converts an UPPERCASE URA-shaped string into Title Case
// for display. Strings that already contain lowercase letters are
// returned unchanged (already display-ready).
func titlecaseURA(s string) string {
	if s == "" || !looksAllUpper(s) {
		return s
	}
	lower := strings.ToLower(s)
	// Split on whitespace runs, preserving them.
	var b strings.Builder
	b.Grow(len(s))
	wordStart := 0
	for i := 0; i <= len(lower); i++ {
		// Word boundary on space, end-of-string.
		if i == len(lower) || lower[i] == ' ' {
			if i > wordStart {
				b.WriteString(titlecaseWord(lower[wordStart:i]))
			}
			if i < len(lower) {
				b.WriteByte(lower[i])
			}
			wordStart = i + 1
		}
	}
	return b.String()
}

// looksAllUpper reports whether s contains at least one letter and all
// its letters are uppercase. Numbers, spaces, punctuation are ignored.
func looksAllUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		hasLetter = true
		if unicode.IsLower(r) {
			return false
		}
	}
	return hasLetter
}

// titlecaseWord title-cases one whitespace-separated word, honouring
// hyphen / apostrophe sub-boundaries and the keepUpper acronym set.
func titlecaseWord(w string) string {
	if w == "" {
		return w
	}
	upper := strings.ToUpper(w)
	if _, ok := keepUpper[upper]; ok {
		return upper
	}
	// Sub-tokenize on apostrophe / hyphen so each segment gets its own
	// leading uppercase ("D'Leedon", "Ang-Mo").
	var b strings.Builder
	b.Grow(len(w))
	upcaseNext := true
	for _, r := range w {
		if upcaseNext && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			upcaseNext = false
			continue
		}
		b.WriteRune(r)
		if r == '\'' || r == '-' {
			upcaseNext = true
		}
	}
	return b.String()
}

// uraStringPtr applies titlecaseURA to a *string, returning nil when
// the input is nil. Convenience for serializing nullable URA columns.
func uraStringPtr(p *string) any {
	if p == nil {
		return nil
	}
	return titlecaseURA(*p)
}

// uraStringSlice applies titlecaseURA to each element of a string
// slice. Returns a non-nil empty slice when the input is empty, so a
// JSON response field built from it serialises as [] rather than null
// (a null would crash a client that does resp.field.map(...)).
func uraStringSlice(ss []string) []string {
	if len(ss) == 0 {
		return []string{}
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = titlecaseURA(s)
	}
	return out
}
