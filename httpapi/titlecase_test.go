package httpapi

import (
	"encoding/json"
	"testing"
)

// TestUraStringSliceNeverNil pins the JSON contract that empty input yields a
// non-nil slice, so a response field built from it serialises as [] not null.
// A null crashes any client doing resp.field.map(...) — the class of bug that
// took down the Articles and admin Users pages in production.
func TestUraStringSliceNeverNil(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got := uraStringSlice(in)
		if got == nil {
			t.Fatalf("uraStringSlice(%#v) returned nil; want non-nil empty slice", in)
		}
		if b, _ := json.Marshal(got); string(b) != "[]" {
			t.Fatalf("uraStringSlice(%#v) marshals to %s; want []", in, b)
		}
	}
}

func TestTitlecaseURA(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Common URA shapes
		{"COASTAL CABANA", "Coastal Cabana"},
		{"THE ORIE", "The Orie"},
		{"BEDOK SOUTH AVENUE 1", "Bedok South Avenue 1"},

		// Acronyms / building-type suffixes preserved
		{"COASTAL CABANA EC", "Coastal Cabana EC"},
		{"PARC CANBERRA EC", "Parc Canberra EC"},

		// Roman numerals preserved
		{"VILLA DELLE ROSE II", "Villa Delle Rose II"},
		{"BAYSHORE PARK III", "Bayshore Park III"},

		// Apostrophes — letter after apostrophe stays uppercase
		{"D'LEEDON", "D'Leedon"},
		{"J'DEN", "J'Den"},

		// Hyphenated proper nouns
		{"ANG-MO-KIO", "Ang-Mo-Kio"},

		// Already-titlecased strings should be left alone
		{"Coastal Cabana", "Coastal Cabana"},
		{"D'Leedon", "D'Leedon"},

		// Mixed-case / lowercase strings (third-party shape) untouched
		{"99-yrs leasehold", "99-yrs leasehold"},
		{"Qingjian Realty Pte Ltd", "Qingjian Realty Pte Ltd"},

		// Edge cases
		{"", ""},
		{"D17", "D17"}, // no letters need casing
		{"123", "123"}, // digits only

		// MRT / agencies
		{"MARINE PARADE MRT", "Marine Parade MRT"},
	}

	for _, c := range cases {
		got := titlecaseURA(c.in)
		if got != c.want {
			t.Errorf("titlecaseURA(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLooksAllUpper(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"COASTAL CABANA", true},
		{"D'LEEDON", true},
		{"COASTAL CABANA EC", true},
		{"Coastal Cabana", false},
		{"coastal cabana", false},
		{"", false},
		{"123", false}, // no letters
		{"D17", true},  // single uppercase letter counts
	}
	for _, c := range cases {
		if got := looksAllUpper(c.in); got != c.want {
			t.Errorf("looksAllUpper(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
