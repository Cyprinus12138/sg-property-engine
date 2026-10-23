package ingest

import (
	"testing"
	"time"
)

func TestParseURAContractDate(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"0324", time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), true},
		{"1219", time.Date(2019, 12, 1, 0, 0, 0, 0, time.UTC), true},
		{" 0125 ", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"", time.Time{}, false},
		{"324", time.Time{}, false},
		{"ab24", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := parseURAContractDate(c.in)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("parseURAContractDate(%q) = (%v,%v) want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseURAMonthPeriod_RejectsBadMonth(t *testing.T) {
	if _, ok := parseURAMonthPeriod("1324"); ok {
		t.Error("month 13 must be rejected")
	}
	if _, ok := parseURAMonthPeriod("0024"); ok {
		t.Error("month 00 must be rejected")
	}
	got, ok := parseURAMonthPeriod("0913")
	if !ok || !got.Equal(time.Date(2013, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parseURAMonthPeriod(0913) = %v,%v", got, ok)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		in     string
		lo, hi float64
		ok     bool
	}{
		{"900-1000", 900, 1000, true},
		{" 1500 - 2000 ", 1500, 2000, true},
		{"85", 85, 85, true},
		{"", 0, 0, false},
		{"1000-900", 0, 0, false}, // inverted band is malformed
		{"a-b", 0, 0, false},
	}
	for _, c := range cases {
		lo, hi, ok := parseRange(map[string]any{"areaSqm": c.in}, "areaSqm")
		if ok != c.ok || lo != c.lo || hi != c.hi {
			t.Errorf("parseRange(%q) = (%v,%v,%v) want (%v,%v,%v)", c.in, lo, hi, ok, c.lo, c.hi, c.ok)
		}
	}
}

func TestParseHDBRecords_SkipsIncompleteRows(t *testing.T) {
	recs := []map[string]any{
		{
			"month": "2024-03", "town": "BISHAN", "block": "123", "street_name": "BISHAN ST 12",
			"flat_type": "4 ROOM", "storey_range": "07 TO 09", "floor_area_sqm": "93",
			"flat_model": "Model A", "lease_commence_date": "1987", "remaining_lease": "62 years",
			"resale_price": "680000",
		},
		{"month": "bad", "resale_price": "1"},    // unparseable month → dropped
		{"month": "2024-03", "resale_price": ""}, // zero price → dropped
	}
	rows := parseHDBRecords(recs)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Town != "BISHAN" || r.FloorAreaSqm != 93 || r.LeaseCommence != 1987 || r.ResalePrice != 680000 {
		t.Errorf("row parsed wrong: %+v", r)
	}
	if !r.Month.Equal(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month = %v", r.Month)
	}
}

func TestParseHDBPropertyInfo_ResidentialOnlyAndSentinels(t *testing.T) {
	recs := []map[string]any{
		{"street": "bishan st 12", "blk_no": "123", "residential": "Y", "year_completed": "1987", "max_floor_lvl": "12", "total_dwelling_units": "uc", "bldg_contract_town": "bsh"},
		{"street": "BISHAN ST 12", "blk_no": "124", "residential": "N"},
		{"street": "", "blk_no": "1", "residential": "Y"},
	}
	rows := parseHDBPropertyInfo(recs)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (non-residential and blank street dropped)", len(rows))
	}
	r := rows[0]
	if r.StreetName != "BISHAN ST 12" || r.TownCode != "BSH" {
		t.Errorf("normalisation: %+v", r)
	}
	if r.YearCompleted == nil || *r.YearCompleted != 1987 || r.MaxFloorLvl == nil || *r.MaxFloorLvl != 12 {
		t.Errorf("ints: %+v", r)
	}
	if r.TotalDwelling != nil {
		t.Errorf("sentinel 'uc' must parse to nil, got %d", *r.TotalDwelling)
	}
}

func TestParseURATxnRecords_FlattensAndComputesPSF(t *testing.T) {
	recs := []map[string]any{{
		"project": "THE SAIL @ MARINA BAY", "street": "MARINA BOULEVARD", "marketSegment": "CCR",
		"transaction": []any{
			map[string]any{"district": "01", "tenure": "99 yrs lease commencing from 2002", "propertyType": "Apartment",
				"area": "100", "price": "2000000", "contractDate": "0324", "floorRange": "41-45", "typeOfSale": "3"},
			map[string]any{"area": "100", "price": "0", "contractDate": "0324"}, // zero price dropped
			"not-a-map",
		},
	}}
	rows := parseURATxnRecords(recs)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	wantPSF := 2000000 / (100 * 10.7639)
	if r.PSF < wantPSF-0.01 || r.PSF > wantPSF+0.01 {
		t.Errorf("psf = %v, want %v", r.PSF, wantPSF)
	}
	if r.MarketSegment != "CCR" || r.District != "01" || r.Project != "THE SAIL @ MARINA BAY" {
		t.Errorf("row: %+v", r)
	}
}

func TestParseURARentalRecords_Bands(t *testing.T) {
	recs := []map[string]any{{
		"project": "X", "street": "Y",
		"rental": []any{
			map[string]any{"district": "15", "propertyType": "Condominium", "areaSqm": "90-100", "rent": "4500", "noOfBedRoom": "3", "leaseDate": "0124"},
			map[string]any{"rent": "4500"}, // no lease date → dropped
		},
	}}
	rows := parseURARentalRecords(recs)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].AreaSqmMin != 90 || rows[0].AreaSqmMax != 100 || rows[0].BedroomNum != 3 {
		t.Errorf("row: %+v", rows[0])
	}
}

func TestCEAParsers(t *testing.T) {
	for _, in := range []string{"-", "--", "NA", "N.A.", "n/a", ""} {
		if got := cleanCEAPlaceholder(in); got != "" {
			t.Errorf("cleanCEAPlaceholder(%q) = %q, want empty", in, got)
		}
	}
	if got := cleanCEAPlaceholder(" Bishan "); got != "Bishan" {
		t.Errorf("trim: %q", got)
	}
	got, ok := parseCEATransactionDate("OCT-2017")
	if !ok || !got.Equal(time.Date(2017, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parseCEATransactionDate(OCT-2017) = %v,%v", got, ok)
	}
	if _, ok := parseCEATransactionDate(""); ok {
		t.Error("empty date must fail")
	}
	d, ok := parseCEADate("05/11/2019")
	if !ok || !d.Equal(time.Date(2019, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parseCEADate DD/MM/YYYY = %v,%v", d, ok)
	}
	if normaliseCEAHeader(" Salesperson_Name ") != "salesperson name" {
		t.Error("header normalisation")
	}
}

func TestStationHelpers(t *testing.T) {
	if normalizeStationName("TANJONG PAGAR MRT STATION") != "TANJONG PAGAR" {
		t.Error("strip MRT STATION suffix")
	}
	if normalizeStationName("  bukit  panjang LRT STATION ") != "BUKIT PANJANG" {
		t.Error("strip LRT suffix + collapse whitespace")
	}
	if normalizeStationName("TANJONG PAGAR (EXIT A)") != "TANJONG PAGAR" {
		t.Error("strip parenthesised suffix")
	}
	cases := map[string]string{"EW15": "EWL", "TE17": "TEL", "CG2": "EWL", "SE1": "SLRT", "ZZ9": "", "X": ""}
	for code, want := range cases {
		if got := lineForCode(code); got != want {
			t.Errorf("lineForCode(%s) = %q, want %q", code, got, want)
		}
	}
	for _, c := range []string{"CC9", "DT18", "NS27", "CG1A"} {
		if !stationCodePattern.MatchString(c) {
			t.Errorf("%s should match stationCodePattern", c)
		}
	}
	for _, c := range []string{"Paya Lebar", "CC", "C9", "cc9"} {
		if stationCodePattern.MatchString(c) {
			t.Errorf("%s should not match stationCodePattern", c)
		}
	}
	lat, lng, ok := parseLatLng("1.3521,103.8198")
	if !ok || lat != 1.3521 || lng != 103.8198 {
		t.Errorf("parseLatLng = %v %v %v", lat, lng, ok)
	}
	if _, _, ok := parseLatLng("nope"); ok {
		t.Error("parseLatLng must reject non-pairs")
	}
}

func TestGeoHelpers(t *testing.T) {
	results := []GeoResult{
		{Lat: 1, Lng: 2, Postal: "NIL", BlkNo: "123", RoadName: "BISHAN ST 12", BuildingName: "NIL"},
		{Lat: 3, Lng: 4, Postal: "570123", BlkNo: "123", RoadName: "BISHAN ST 12", BuildingName: "BISHAN LOFT"},
		{Lat: 5, Lng: 6, Postal: "570124", BlkNo: "124", RoadName: "OTHER RD"},
	}
	if m := matchHDBBlock(results, "123", "bishan st 12"); m == nil || m.Lat != 1 {
		t.Errorf("matchHDBBlock should return the first exact BLK/ROAD match, got %+v", m)
	}
	if m := matchHDBBlock(results, "999", "X"); m != nil {
		t.Error("no match expected")
	}
	if got := filterByStreet(results, "BISHAN ST 12"); len(got) != 2 {
		t.Errorf("filterByStreet = %d rows, want 2", len(got))
	}
	if got := filterByStreet(results, ""); len(got) != 3 {
		t.Error("empty street must pass everything through")
	}
	if best := pickBestResult(results); best.Postal != "570123" {
		t.Errorf("pickBestResult should prefer a usable postal, got %+v", best)
	}
	b := buildingsFromResults("P", results)
	if len(b) != 2 || b[0].Postal != "570123" || b[1].Postal != "570124" {
		t.Errorf("buildingsFromResults dedupe by postal: %+v", b)
	}
	if hasUsablePostal(" nil ") || hasUsablePostal("") || !hasUsablePostal("570123") {
		t.Error("hasUsablePostal")
	}
}
