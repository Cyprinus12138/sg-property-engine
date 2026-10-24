package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
)

// The ops UI is three static files compiled into the binary. It talks to
// the same JSON endpoints as any other client and has no server-side state.
//
//go:embed ui
var uiFiles embed.FS

func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // the embed path is fixed at compile time
	}
	files := http.StripPrefix("/ui/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Small files, and a stale app.js after an upgrade would call
		// endpoints the old binary did not have: always revalidate.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		files.ServeHTTP(w, r)
	})
}
