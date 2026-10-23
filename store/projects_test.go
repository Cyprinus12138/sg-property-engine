package store

import (
	"reflect"
	"testing"
)

func TestIsPipelinePlaceholder(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Real project names — must NOT be flagged.
		{"Newport Residences", false},
		{"51 Anson/Anson Centre", false},
		{"J'den", false},
		{"The Sail @ Marina Bay", false},
		{"Belgravia Ace", false},

		// URA placeholder labels — must be flagged.
		{"Service apartments", true},
		{"Residential apartments", true},
		{"Residential/service apartments/Retail development", true},
		{"Service apartments/Hotel/retail development", true},
		{"Mixed development", true},
		{"Mixed-use development", true},
		{"Executive condominium development", true},
		{"Landed housing development", true},
		{"Apartment development", true},
		{"Townhouse development", true},
		{"Detached housing development", true},
		{"Semi-detached housing development", true},
		{"Terrace housing development", true},
		{"Cluster housing", true},
		{"Shophouses", true},
		{"Redevelopment", true},

		// Edge cases.
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		got := IsPipelinePlaceholder(c.in)
		if got != c.want {
			t.Errorf("IsPipelinePlaceholder(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPipelinePropertyTypes(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want []string
	}{
		{
			name: "condo with apartments",
			raw: map[string]any{
				"noOfApartment": float64(120),
				"noOfCondo":     float64(50),
			},
			want: []string{"Apartment", "Condominium"},
		},
		{
			name: "all zero",
			raw: map[string]any{
				"noOfApartment": float64(0),
				"noOfCondo":     float64(0),
			},
			want: nil,
		},
		{
			name: "string-typed counts (defensive)",
			raw: map[string]any{
				"noOfTerrace":       "12",
				"noOfDetachedHouse": "0",
			},
			want: []string{"Terrace"},
		},
		{
			name: "missing keys",
			raw:  map[string]any{},
			want: nil,
		},
		{
			name: "nil raw",
			raw:  nil,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pipelinePropertyTypes(URAPipelineRow{Raw: c.raw})
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
