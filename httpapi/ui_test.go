package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

func TestUI_ServesEmbeddedFiles(t *testing.T) {
	h := (&Server{}).Routes()
	for path, wantType := range map[string]string{
		"/ui/":        "text/html",
		"/ui/app.js":  "javascript",
		"/ui/app.css": "text/css",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s → %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("GET %s content-type = %q, want %s", path, ct, wantType)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s: no CSP header", path)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET / → %d %q, want redirect to /ui/", rec.Code, rec.Header().Get("Location"))
	}
	// The root redirect must not swallow unknown paths.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("GET /nope → %d, want 404", rec.Code)
	}
}

// Every endpoint app.js calls must exist on the mux, with the method it
// uses. Catches a renamed route that would leave the UI quietly broken.
func TestUI_CallsOnlyRegisteredRoutes(t *testing.T) {
	js, err := uiFiles.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// api(`../path?...`, { method: "POST" }) — capture path and an optional method.
	re := regexp.MustCompile("api\\([`\"]\\.\\.(/[^`\"?$]*(?:\\$\\{[^}]+\\}[^`\"?$]*)*)([^)]*)\\)")
	methodRe := regexp.MustCompile(`method: "([A-Z]+)"`)
	calls := re.FindAllStringSubmatch(string(js), -1)
	if len(calls) < 5 {
		t.Fatalf("found only %d api() calls in app.js; the pattern is out of date", len(calls))
	}
	mux := (&Server{}).mux()
	for _, c := range calls {
		path := regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(c[1], "x")
		method := "GET"
		if m := methodRe.FindStringSubmatch(c[2]); m != nil {
			method = m[1]
		}
		_, pattern := mux.Handler(httptest.NewRequest(method, path, nil))
		if pattern == "" || pattern == "GET /{$}" || strings.HasPrefix(pattern, "GET /ui/") {
			t.Errorf("app.js calls %s %s, which matches no API route (pattern %q)", method, c[1], pattern)
		}
	}
}

func TestDBTableRows_UnknownTableIs404(t *testing.T) {
	// The allowlist check runs before any query, so no database is needed.
	h := (&Server{Store: &store.Store{}}).Routes()
	for _, table := range []string{"pg_authid", "hdb_resale;drop", "sync_runs%20--"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/db/tables/"+table+"/rows", nil))
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "unknown_table") {
			t.Errorf("table %q → %d %s, want 404 unknown_table", table, rec.Code, rec.Body.String())
		}
	}
}
