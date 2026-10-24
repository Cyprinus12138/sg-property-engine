package mcpserver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The query API filters by exact match on values stored the way the
// publishers emit them ("BISHAN", "4 ROOM", district "05"). A model will
// write "Bishan", "4-room", "D5". Normalising here, in code, is what makes
// the filters usable; telling the model the format in a description is not
// enough on its own.

// normUpper uppercases and collapses whitespace. Used for HDB towns,
// streets and blocks, and URA project names, all stored uppercase.
func normUpper(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), " "))
}

var districtRe = regexp.MustCompile(`(?i)^\s*(?:d|district)?\s*0*(\d{1,2})\s*$`)

// normDistrict turns "D5", "district 05", "5" into URA's "05". Postal
// districts run 01–28.
func normDistrict(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	m := districtRe.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("district %q: want a postal district number 1-28, e.g. \"15\" or \"D15\"", s)
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > 28 {
		return "", fmt.Errorf("district %q: Singapore postal districts are 1-28", s)
	}
	return fmt.Sprintf("%02d", n), nil
}

var monthRe = regexp.MustCompile(`^(\d{4})-(\d{2})(?:-(\d{2}))?$`)

// normDate accepts YYYY-MM or YYYY-MM-DD. A bare month becomes its first
// day, which is how every month column is stored.
func normDate(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	m := monthRe.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("%s %q: want YYYY-MM or YYYY-MM-DD", field, s)
	}
	if mo, _ := strconv.Atoi(m[2]); mo < 1 || mo > 12 {
		return "", fmt.Errorf("%s %q: month out of range", field, s)
	}
	if m[3] == "" {
		return s + "-01", nil
	}
	return s, nil
}

var roomRe = regexp.MustCompile(`^([1-5])\s*-?\s*(?:ROOMS?|RM|R)?$`)

// normFlatType maps "4-room", "4rm", "4 Room" to HDB's "4 ROOM", and the
// executive / multi-generation spellings to theirs.
func normFlatType(s string) string {
	u := normUpper(strings.NewReplacer("_", " ").Replace(s))
	if u == "" {
		return ""
	}
	if m := roomRe.FindStringSubmatch(u); m != nil {
		return m[1] + " ROOM"
	}
	switch strings.ReplaceAll(strings.ReplaceAll(u, "-", ""), " ", "") {
	case "EXEC", "EXECUTIVE", "EA", "EM":
		return "EXECUTIVE"
	case "MULTIGENERATION", "MULTIGEN", "MG":
		return "MULTI-GENERATION"
	}
	return u
}

// URA property types as published, plus the two labels the pipeline feed
// contributes to projects.property_types.
var propertyTypes = []string{
	"Apartment", "Condominium", "Executive Condominium",
	"Detached", "Semi-detached", "Terrace",
	"Strata Detached", "Strata Semi-detached", "Strata Terrace",
	"Detached House", "Semi-Detached House",
	// URA's rental feed groups property types differently.
	"Non-landed Properties", "Terrace House",
}

// normPropertyType restores URA's casing ("condominium" → "Condominium")
// and expands "EC". Unknown values pass through for the API to match.
func normPropertyType(s string) string {
	t := strings.Join(strings.Fields(s), " ")
	if strings.EqualFold(t, "EC") {
		return "Executive Condominium"
	}
	if strings.EqualFold(t, "condo") {
		return "Condominium"
	}
	for _, p := range propertyTypes {
		if strings.EqualFold(p, t) {
			return p
		}
	}
	return t
}

// normSegment uppercases a market segment and checks it is one of URA's
// three.
func normSegment(s string) (string, error) {
	u := normUpper(s)
	switch u {
	case "", "CCR", "RCR", "OCR":
		return u, nil
	}
	return "", fmt.Errorf("market_segment %q: want CCR, RCR or OCR", s)
}

// clampLimit applies the tool-level page size. Rows are wide and land in a
// model's context; the HTTP API allows far more.
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	}
	return n
}

const (
	defaultLimit = 20
	maxLimit     = 100
)
