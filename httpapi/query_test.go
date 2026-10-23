package httpapi

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeAnchorStore satisfies the ResolveAnchor seam applyNearAnchor needs.
type fakeAnchorStore struct {
	lat, lng float64
	ok       bool
	calls    [][3]string
}

func (f *fakeAnchorStore) ResolveAnchor(_ context.Context, kind, a, b string) (float64, float64, bool, error) {
	f.calls = append(f.calls, [3]string{kind, a, b})
	return f.lat, f.lng, f.ok, nil
}

func TestQB_FiltersNumberPlaceholdersInOrder(t *testing.T) {
	q := newQB("p.geom")
	q.eq("t.district", "15")
	q.eq("t.tenure", "") // empty → no clause
	q.prefix("t.project", "THE")
	q.gte("t.price", "1000000")
	q.lte("t.price", "")
	q.anyElem("property_types", "Condominium")

	want := []string{
		"t.district = $1",
		"t.project ILIKE $2",
		"t.price >= $3",
		"$4 = ANY(property_types)",
	}
	if !reflect.DeepEqual(q.where, want) {
		t.Fatalf("where = %v, want %v", q.where, want)
	}
	if got, want := q.args, []any{"15", "THE%", "1000000", "Condominium"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if got := q.whereClause(); !strings.HasPrefix(got, " WHERE ") || !strings.Contains(got, " AND ") {
		t.Fatalf("whereClause = %q", got)
	}
	if newQB("x").whereClause() != "" {
		t.Fatal("empty qb must produce an empty where clause")
	}
}

func TestQB_NearAnchorRawCoordinatesAndRadiusClamp(t *testing.T) {
	q := newQB("b.geom")
	q.eq("r.town", "BISHAN")
	r := httptest.NewRequest("GET", "/hdb/transactions?near_lat=1.35&near_lng=103.85&radius_m=99999", nil)
	if err := q.applyNearAnchor(context.Background(), &fakeAnchorStore{}, r); err != nil {
		t.Fatal(err)
	}
	// Args: town, lng, lat, radius (clamped to 5000).
	if got, want := q.args, []any{"BISHAN", 103.85, 1.35, 5000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if q.anchorGeom != "ST_SetSRID(ST_MakePoint($2, $3), 4326)::geography" {
		t.Fatalf("anchorGeom = %q", q.anchorGeom)
	}
	if last := q.where[len(q.where)-1]; last != "ST_DWithin(b.geom, ST_SetSRID(ST_MakePoint($2, $3), 4326)::geography, $4)" {
		t.Fatalf("dwithin clause = %q", last)
	}
}

func TestQB_NearAnchorResolvesNamedAnchors(t *testing.T) {
	cases := []struct {
		query string
		want  [3]string
	}{
		{"near_project=THE+SAIL", [3]string{"project", "THE SAIL", ""}},
		{"near_poi_type=mrt_exit&near_poi_name=Bishan", [3]string{"poi", "mrt_exit", "Bishan"}},
		{"near_block=123&near_street=BISHAN+ST+12", [3]string{"hdb_block", "BISHAN ST 12", "123"}},
	}
	for _, c := range cases {
		fs := &fakeAnchorStore{lat: 1.3, lng: 103.8, ok: true}
		q := newQB("p.geom")
		r := httptest.NewRequest("GET", "/projects?"+c.query, nil)
		if err := q.applyNearAnchor(context.Background(), fs, r); err != nil {
			t.Fatal(err)
		}
		if len(fs.calls) != 1 || fs.calls[0] != c.want {
			t.Errorf("%s: ResolveAnchor calls = %v, want %v", c.query, fs.calls, c.want)
		}
		if q.anchorGeom == "" {
			t.Errorf("%s: anchor not applied", c.query)
		}
		// Default radius when unspecified.
		if got := q.args[len(q.args)-1]; got != 1000 {
			t.Errorf("%s: default radius = %v, want 1000", c.query, got)
		}
	}

	// Unresolvable anchor → no geo clause, no error.
	q := newQB("p.geom")
	r := httptest.NewRequest("GET", "/projects?near_project=NOPE", nil)
	if err := q.applyNearAnchor(context.Background(), &fakeAnchorStore{ok: false}, r); err != nil {
		t.Fatal(err)
	}
	if q.anchorGeom != "" || len(q.where) != 0 {
		t.Fatalf("unresolved anchor must leave the query untouched: %v", q.where)
	}
}

func TestQB_HasPOIClause(t *testing.T) {
	q := newQB("projects.geom")
	r := httptest.NewRequest("GET", "/projects?has_poi_type=school&has_poi_within_m=0&has_poi_subtype=PRIMARY", nil)
	q.applyHasPOI(r)
	if got, want := q.args, []any{"school", 500, "PRIMARY"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v (within_m<=0 defaults to 500)", got, want)
	}
	clause := q.where[0]
	for _, frag := range []string{"EXISTS (", "FROM pois p", "p.type = $1", "ST_DWithin(projects.geom, p.geom, $2)", "p.subtype = $3"} {
		if !strings.Contains(clause, frag) {
			t.Errorf("clause missing %q:\n%s", frag, clause)
		}
	}
	// Absent → no-op.
	q2 := newQB("x")
	q2.applyHasPOI(httptest.NewRequest("GET", "/projects", nil))
	if len(q2.where) != 0 {
		t.Fatal("has_poi absent must not add a clause")
	}
}

func TestPagination_Clamps(t *testing.T) {
	cases := []struct {
		query          string
		wantL, wantOff int
	}{
		{"", 50, 0},
		{"limit=10&offset=20", 10, 20},
		{"limit=9999", 500, 0},
		{"limit=-1&offset=-5", 50, 0},
		{"limit=abc", 50, 0},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x?"+c.query, nil)
		l, off := pagination(r, defaultPageSize, maxPageSize)
		if l != c.wantL || off != c.wantOff {
			t.Errorf("%q: got (%d,%d) want (%d,%d)", c.query, l, off, c.wantL, c.wantOff)
		}
	}
}

func TestMergeSearchRows_InterleavesByRank(t *testing.T) {
	p := func(name string, rank int) searchRow { return searchRow{Kind: "project", Name: name, rank: rank} }
	h := func(name string, rank int) searchRow { return searchRow{Kind: "hdb", Name: name, rank: rank} }

	projects := []searchRow{p("P0", 0), p("P1", 1), p("P2", 2)}
	hdb := []searchRow{h("H1", 1), h("H2", 2), h("H2b", 2)}

	got := mergeSearchRows(projects, hdb, 10)
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Name)
	}
	// Exact-match project first; on tied rank the merge alternates by output
	// position so both kinds stay visible in a short dropdown.
	want := []string{"P0", "H1", "P1", "H2", "P2", "H2b"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("merged = %v, want %v", names, want)
	}

	if got := mergeSearchRows(projects, hdb, 2); len(got) != 2 {
		t.Fatalf("limit not honoured: %d rows", len(got))
	}
	if got := mergeSearchRows(nil, hdb, 10); len(got) != 3 || got[0].Kind != "hdb" {
		t.Fatalf("one-sided merge broken: %+v", got)
	}
}

func TestHDBDisplayNameAndSubtitle(t *testing.T) {
	blk, st, est := "123", "BISHAN ST 12", "Bishan Loft"
	if got := hdbDisplayName(&blk, &st, &est); got != "Blk 123 BISHAN ST 12" {
		t.Errorf("display = %q", got)
	}
	if got := hdbDisplayName(nil, nil, &est); got != "Bishan Loft" {
		t.Errorf("estate fallback = %q", got)
	}
	postal, pa := "570123", "BISHAN"
	if got := hdbSubtitle(&est, &postal, &pa); got != "Bishan Loft · BISHAN · 570123" {
		t.Errorf("subtitle = %q", got)
	}
	d := "15"
	tenure := "99-year leasehold"
	if got := projectSubtitle(&searchRow{District: &d, PropertyTypes: []string{"Condominium"}, Tenure: &tenure}); got != "District 15 · Condominium · 99-year leasehold" {
		t.Errorf("project subtitle = %q", got)
	}
}

func TestPSFDirection(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		recent, prior *float64
		wantDir       string
		wantPct       *float64
	}{
		{nil, f(1000), "flat", nil},
		{f(1000), nil, "flat", nil},
		{f(1000), f(0), "flat", nil},
		{f(1100), f(1000), "up", f(10)},
		{f(900), f(1000), "down", f(-10)},
		{f(1000.5), f(1000), "flat", f(0.05)},
	}
	for _, c := range cases {
		dir, pct := psfDirection(c.recent, c.prior)
		if dir != c.wantDir {
			t.Errorf("dir = %s, want %s (recent=%v prior=%v)", dir, c.wantDir, c.recent, c.prior)
		}
		if (pct == nil) != (c.wantPct == nil) {
			t.Errorf("pct nil-ness mismatch for recent=%v prior=%v", c.recent, c.prior)
			continue
		}
		if pct != nil && (*pct-*c.wantPct) > 1e-9 && (*c.wantPct-*pct) > 1e-9 {
			t.Errorf("pct = %v, want %v", *pct, *c.wantPct)
		}
	}
}
