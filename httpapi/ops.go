package httpapi

import (
	"errors"
	"net/http"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

// syncRuns — GET /sync/runs[?source=]: run history, newest first.
func (s *Server) syncRuns(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r, defaultPageSize, maxPageSize)
	runs, total, err := s.Store.ListRuns(r.Context(), r.URL.Query().Get("source"), limit, offset)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"rows": runs, "total": total, "limit": limit, "offset": offset})
}

// dbTables — GET /db/tables: row count, size and last write per table.
func (s *Server) dbTables(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.TableStats(r.Context())
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tables": stats})
}

// dbTableRows — GET /db/tables/{table}/rows: raw rows in a stable order.
// Paged at the preview size; this is for eyeballing, not export.
func (s *Server) dbTableRows(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r, previewPageDefault, previewPageMax)
	p, err := s.Store.PreviewRows(r.Context(), r.PathValue("table"), limit, offset)
	if errors.Is(err, store.ErrUnknownTable) {
		writeErr(w, 404, "unknown_table", "no such table")
		return
	}
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, p)
}
