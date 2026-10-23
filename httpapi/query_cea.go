package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

// ceaLookup resolves a CEA salesperson registration number to the most
// recent registration row. Used by the public-facing app to validate the
// number a prospective agent enters during signup, and to auto-fill their
// agency name / license. Read-only, no PII beyond what data.gov.sg already
// publishes; safe to expose through the /dataengine reverse proxy.
//
// 404 means the registration number is not in the imported CEA snapshot;
// the caller should treat that as "invalid".
func (s *Server) ceaLookup(w http.ResponseWriter, r *http.Request) {
	regNo := strings.TrimSpace(r.URL.Query().Get("registration_no"))
	if regNo == "" {
		writeErr(w, 400, "missing_registration_no", "registration_no is required")
		return
	}
	hit, err := s.Store.FindCEASalesperson(r.Context(), regNo)
	if err != nil {
		if errors.Is(err, store.ErrCEANotFound) {
			writeErr(w, 404, "not_found", "registration number not found")
			return
		}
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, hit)
}

// ceaPracticeStats serves the Account → Practice statistics card. Reads
// from cea_transactions (data.gov.sg d_ee7e46d3c57f7865790704632b0aef71)
// aggregated to a 12-month rolling window for one agent, split by
// sale/rental mode. The shape — and the choice of "active months" in
// place of a dollar-value KPI — is driven by what the upstream dataset
// actually exposes (no transaction price column).
//
// 200 on success even when the agent has zero rows; the FE renders an
// empty state rather than asking the user to retry. 400 only on a
// malformed query, 404 when the registration number itself is missing
// from the salesperson register AND has no transactions on file.
func (s *Server) ceaPracticeStats(w http.ResponseWriter, r *http.Request) {
	regNo := strings.TrimSpace(r.URL.Query().Get("reg_no"))
	if regNo == "" {
		writeErr(w, 400, "missing_reg_no", "reg_no is required")
		return
	}
	mode := r.URL.Query().Get("mode")
	stats, err := s.Store.GetCEAPracticeStats(r.Context(), regNo, mode)
	if err != nil {
		if errors.Is(err, store.ErrCEANotFound) {
			writeErr(w, 404, "not_found", "registration number not found")
			return
		}
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, stats)
}
