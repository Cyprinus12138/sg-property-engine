// Package httpapi serves the engine's JSON query API and the ops
// endpoints that drive the scheduler. One mux, one listener.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/scheduler"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// Server holds the dependencies every handler needs. Construct it with
// struct literals; there is no hidden state.
type Server struct {
	Store *store.Store
	Sched *scheduler.Scheduler
}

// Routes builds the HTTP handler. Paths are the public contract — see
// README.md for the table — and are kept stable across releases.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Ops
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /sync/status", s.syncStatus)
	mux.HandleFunc("POST /sync/run", s.syncRun)
	mux.HandleFunc("GET /sync/runs/{id}", s.syncRunByID)

	// Query — HDB
	mux.HandleFunc("GET /hdb/transactions", s.hdbTransactions)
	mux.HandleFunc("GET /hdb/stats", s.hdbStats)

	// Query — URA
	mux.HandleFunc("GET /ura/transactions", s.uraTransactions)
	mux.HandleFunc("GET /ura/rentals", s.uraRentals)
	mux.HandleFunc("GET /ura/rentals/trend", s.uraRentalsTrend)
	mux.HandleFunc("GET /ura/pipeline", s.uraPipeline)
	mux.HandleFunc("GET /ura/developer-sales", s.uraDeveloperSales)

	// Query — projects & POIs
	mux.HandleFunc("GET /projects", s.projects)
	mux.HandleFunc("GET /projects/{name}", s.projectByName)
	mux.HandleFunc("GET /pois", s.pois)
	mux.HandleFunc("GET /pois/types", s.poiTypes)
	mux.HandleFunc("GET /geo/nearest-stations", s.nearestStations)

	// Unified property autocomplete (projects + HDB blocks).
	mux.HandleFunc("GET /search/properties", s.searchProperties)

	// Explore — aggregations.
	mux.HandleFunc("GET /explore/hottest", s.hottestProjects)
	mux.HandleFunc("GET /explore/district_median", s.districtMedian)

	// Query — CEA salesperson register
	mux.HandleFunc("GET /cea/lookup", s.ceaLookup)
	mux.HandleFunc("GET /cea/practice-stats", s.ceaPracticeStats)

	return logMiddleware(mux)
}

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rw := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(rw, r)
		// /healthz is hit every few seconds by container healthchecks and
		// is pure noise in a log aggregator — skip it. The request still
		// serves normally.
		if r.URL.Path == "/healthz" {
			return
		}
		slog.Info("http",
			"method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
			"status", rw.code, "duration_ms", time.Since(started).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": msg, "code": errCode})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeErr(w, 503, "db_unreachable", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) syncStatus(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.LatestRuns(r.Context())
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"sources": runs,
		"known":   s.Sched.Sources(),
	})
}

func (s *Server) syncRun(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = store.ModeIncremental
	}
	// `arg` is the optional per-trigger argument forwarded to a RunArg
	// source. Ignored by plain sources.
	arg := r.URL.Query().Get("arg")
	id, err := s.Sched.TriggerArg(r.Context(), source, mode, arg)
	if err == store.ErrLocked {
		writeErr(w, 409, "already_running", err.Error())
		return
	}
	if err != nil {
		writeErr(w, 400, "trigger_failed", err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"run_id": id, "source": source, "mode": mode})
}

func (s *Server) syncRunByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "id must be an integer")
		return
	}
	run, err := s.Store.GetRun(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	if run == nil {
		writeErr(w, 404, "not_found", "run not found")
		return
	}
	writeJSON(w, 200, run)
}
