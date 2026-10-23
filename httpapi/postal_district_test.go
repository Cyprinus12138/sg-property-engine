package httpapi

import "testing"

func TestPostalToDistrict(t *testing.T) {
	cases := []struct {
		postal, want string
	}{
		{"018956", "01"}, // Marina Bay
		{"079903", "02"}, // Tanjong Pagar
		{"150035", "03"},
		{"099253", "04"}, // Sentosa / HarbourFront
		{"238858", "09"}, // Orchard
		{"427716", "15"}, // Katong
		{"510214", "18"}, // Pasir Ris
		{"569830", "20"}, // Bishan
		{"819642", "17"}, // Changi (81 sector)
		{"828761", "19"}, // Punggol (82 sector)
		{"730001", "25"},
		{"797651", "28"},
		// Unknown / malformed
		{"", ""},
		{"7", ""},
		{"NIL", ""},
		{"740000", ""}, // sector 74 is not assigned
		{"ab1234", ""},
		{"  569830 ", "20"}, // trimmed
	}
	for _, c := range cases {
		if got := postalToDistrict(c.postal); got != c.want {
			t.Errorf("postalToDistrict(%q) = %q, want %q", c.postal, got, c.want)
		}
	}
}
