package store

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// CEASalespersonRow is one CEA salesperson registration record. The source
// dataset is a current-state snapshot — exactly one row per person, keyed
// on registration_no. Agency switches show up as updates to that row in
// the next refresh, not as additional rows.
type CEASalespersonRow struct {
	SalespersonName       string    `json:"salesperson_name"`
	RegistrationNo        string    `json:"registration_no"`
	RegistrationStartDate time.Time `json:"registration_start_date"`
	RegistrationEndDate   time.Time `json:"registration_end_date"`
	EstateAgentName       string    `json:"estate_agent_name"`
	EstateAgentLicenseNo  string    `json:"estate_agent_license_no"`
}

// CEASalespersonLookup is the latest registration row found for a given
// registration number. `Active` is true when today is within the
// [start, end] window — callers (signup validation) treat inactive hits
// as a soft "registered but lapsed" state.
type CEASalespersonLookup struct {
	SalespersonName       string    `json:"salesperson_name"`
	RegistrationNo        string    `json:"registration_no"`
	RegistrationStartDate time.Time `json:"registration_start_date"`
	RegistrationEndDate   time.Time `json:"registration_end_date"`
	EstateAgentName       string    `json:"estate_agent_name"`
	EstateAgentLicenseNo  string    `json:"estate_agent_license_no"`
	Active                bool      `json:"active"`
}

// ErrCEANotFound is returned by FindCEASalesperson when the supplied
// registration number is not present in the imported snapshot.
var ErrCEANotFound = errors.New("cea: registration number not found")

// FindCEASalesperson resolves a CEA registration number. registration_no
// is the table's primary key, so this is a single-row equality lookup.
func (s *Store) FindCEASalesperson(ctx context.Context, registrationNo string) (*CEASalespersonLookup, error) {
	registrationNo = strings.ToUpper(strings.TrimSpace(registrationNo))
	if registrationNo == "" {
		return nil, ErrCEANotFound
	}
	var (
		out   CEASalespersonLookup
		start *time.Time
		end   *time.Time
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT salesperson_name, registration_no, registration_start_date,
		       registration_end_date, estate_agent_name, estate_agent_license_no
		  FROM cea_salespersons
		 WHERE registration_no = $1`, registrationNo).Scan(
		&out.SalespersonName, &out.RegistrationNo, &start, &end,
		&out.EstateAgentName, &out.EstateAgentLicenseNo,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCEANotFound
		}
		return nil, err
	}
	if start != nil {
		out.RegistrationStartDate = *start
	}
	if end != nil {
		out.RegistrationEndDate = *end
	}
	now := time.Now()
	out.Active = (start == nil || !now.Before(*start)) && (end == nil || !now.After(*end))
	return &out, nil
}

// CEATransactionRow is one agent-attributable transaction from the
// "CEA Salespersons' Property Transaction Records (residential)" feed.
// The dataset has no day-of-month granularity — TransactionMonth is the
// first of the published month. Snapshot dataset: idempotency is enforced
// by source_hash UNIQUE.
type CEATransactionRow struct {
	SalespersonRegNum string    `json:"salesperson_reg_num"`
	SalespersonName   string    `json:"salesperson_name"`
	TransactionMonth  time.Time `json:"transaction_month"`
	PropertyType      string    `json:"property_type"`
	TransactionType   string    `json:"transaction_type"`
	Represented       string    `json:"represented"`
	Town              string    `json:"town"`
	District          string    `json:"district"`
	GeneralLocation   string    `json:"general_location"`
}

// HashCEATransaction is the idempotent upsert key. The natural-key set is
// every field — the source has no transaction id and the same agent can
// close multiple deals in the same month with otherwise-identical metadata,
// so collapsing on a smaller key would silently drop real rows.
func HashCEATransaction(r CEATransactionRow) []byte {
	h := md5.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s|%s|%s",
		r.SalespersonRegNum,
		r.TransactionMonth.Format("2006-01"),
		r.PropertyType, r.TransactionType, r.Represented,
		r.Town, r.District, r.GeneralLocation,
		r.SalespersonName,
	)
	return h.Sum(nil)
}

// UpsertCEATransactions bulk-inserts CEA transaction rows. Re-ingesting the
// same snapshot is a no-op via source_hash. Returns (rowsIn, rowsInserted).
func (s *Store) UpsertCEATransactions(ctx context.Context, rows []CEATransactionRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	inserted := 0
	const stmt = `
		INSERT INTO cea_transactions
		   (salesperson_reg_num, salesperson_name, transaction_month,
		    property_type, transaction_type, represented,
		    town, district, general_location, source_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source_hash) DO NOTHING`
	for _, r := range rows {
		regNo := strings.ToUpper(strings.TrimSpace(r.SalespersonRegNum))
		if regNo == "" || r.TransactionMonth.IsZero() {
			continue
		}
		tag, err := tx.Exec(ctx, stmt,
			regNo, r.SalespersonName, r.TransactionMonth,
			nullIfEmpty(r.PropertyType), nullIfEmpty(r.TransactionType),
			nullIfEmpty(r.Represented),
			nullIfEmpty(r.Town), nullIfEmpty(r.District), nullIfEmpty(r.GeneralLocation),
			HashCEATransaction(CEATransactionRow{
				SalespersonRegNum: regNo,
				SalespersonName:   r.SalespersonName,
				TransactionMonth:  r.TransactionMonth,
				PropertyType:      r.PropertyType,
				TransactionType:   r.TransactionType,
				Represented:       r.Represented,
				Town:              r.Town,
				District:          r.District,
				GeneralLocation:   r.GeneralLocation,
			}),
		)
		if err != nil {
			return 0, 0, err
		}
		inserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(rows), inserted, nil
}

func nullIfEmpty(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// CEAPracticeStats is the per-agent activity rollup surfaced on the
// Account → Practice statistics card. Built entirely from cea_transactions
// (which has no transaction price), so dollar metrics are not part of the
// shape — callers display "active months" instead.
type CEAPracticeStats struct {
	RegistrationNo string           `json:"registration_no"`
	Mode           string           `json:"mode"` // "sale" | "rent"
	WindowFrom     time.Time        `json:"window_from"`
	WindowTo       time.Time        `json:"window_to"`
	Deals          int              `json:"deals"`
	PriorDeals     int              `json:"prior_deals"`
	ActiveMonths   int              `json:"active_months"`
	LicensedSince  *time.Time       `json:"licensed_since,omitempty"`
	Monthly        []CEAMonthlyBin  `json:"monthly"`
	Towns          []CEATownBin     `json:"towns"`
	Representation []CEACategoryBin `json:"representation"`
	TxTypes        []CEACategoryBin `json:"tx_types"`
}

type CEAMonthlyBin struct {
	Month time.Time `json:"month"`
	Count int       `json:"count"`
}

type CEATownBin struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type CEACategoryBin struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// GetCEAPracticeStats aggregates cea_transactions for one agent over a
// 12-month rolling window. mode splits the dataset:
//   - "rent": transaction_type ILIKE '%RENT%'
//   - "sale": everything else (resale / new sale / sub-sale / etc.)
//
// The split is permissive on purpose — the data.gov.sg dataset's exact
// transaction_type vocabulary isn't versioned, so we match by substring
// rather than enumerating literals that could drift.
//
// Top towns are capped at 5 with an "Other" bucket for the rest, so the
// FE renders a fixed-height list without scrolling. Monthly bins are
// zero-filled across all 12 window months so the bar chart never has
// gaps.
func (s *Store) GetCEAPracticeStats(ctx context.Context, registrationNo, mode string) (*CEAPracticeStats, error) {
	regNo := strings.ToUpper(strings.TrimSpace(registrationNo))
	if regNo == "" {
		return nil, ErrCEANotFound
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "rent" {
		mode = "sale"
	}

	now := time.Now().UTC()
	windowTo := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	windowFrom := windowTo.AddDate(-1, 0, 0).AddDate(0, 1, 0) // 12 months inclusive ending at windowTo
	priorFrom := windowFrom.AddDate(-1, 0, 0)

	// `mode_match` is a server-side classifier so all rollup branches
	// agree on the same row partition. Kept inline (no CTE materialise)
	// because Postgres folds the CASE into the WHERE on each branch.
	const modeExpr = `(CASE WHEN transaction_type ILIKE '%RENT%' THEN 'rent' ELSE 'sale' END)`

	stats := &CEAPracticeStats{
		RegistrationNo: regNo,
		Mode:           mode,
		WindowFrom:     windowFrom,
		WindowTo:       windowTo,
		Monthly:        make([]CEAMonthlyBin, 0, 12),
		Towns:          []CEATownBin{},
		Representation: []CEACategoryBin{},
		TxTypes:        []CEACategoryBin{},
	}

	// Licensed-since: best-effort enrichment from the salesperson register.
	// A missing roster row is not fatal — agents who lapsed but still have
	// historical transactions still get a stats panel, just without tenure.
	var licensed *time.Time
	if err := s.Pool.QueryRow(ctx, `
		SELECT registration_start_date FROM cea_salespersons
		 WHERE registration_no = $1`, regNo).Scan(&licensed); err == nil {
		stats.LicensedSince = licensed
	}

	// Window deals (current 12mo) + active-months in one pass.
	var deals, activeMonths int
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT transaction_month)
		  FROM cea_transactions
		 WHERE salesperson_reg_num = $1
		   AND transaction_month >= $2
		   AND transaction_month <  $3
		   AND `+modeExpr+` = $4`,
		regNo, windowFrom, windowTo.AddDate(0, 1, 0), mode,
	).Scan(&deals, &activeMonths); err != nil {
		return nil, fmt.Errorf("cea practice stats deals: %w", err)
	}
	stats.Deals = deals
	stats.ActiveMonths = activeMonths

	// Prior-window deals — used by the FE to render "+X% vs prior 12mo".
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM cea_transactions
		 WHERE salesperson_reg_num = $1
		   AND transaction_month >= $2
		   AND transaction_month <  $3
		   AND `+modeExpr+` = $4`,
		regNo, priorFrom, windowFrom, mode,
	).Scan(&stats.PriorDeals); err != nil {
		return nil, fmt.Errorf("cea practice stats prior: %w", err)
	}

	// Monthly bins. Zero-fill across the window so the chart has a stable
	// 12-bar layout even for low-activity agents.
	rows, err := s.Pool.Query(ctx, `
		SELECT transaction_month, COUNT(*)
		  FROM cea_transactions
		 WHERE salesperson_reg_num = $1
		   AND transaction_month >= $2
		   AND transaction_month <  $3
		   AND `+modeExpr+` = $4
		 GROUP BY transaction_month`,
		regNo, windowFrom, windowTo.AddDate(0, 1, 0), mode,
	)
	if err != nil {
		return nil, fmt.Errorf("cea practice stats monthly: %w", err)
	}
	monthCounts := make(map[time.Time]int, 12)
	for rows.Next() {
		var m time.Time
		var c int
		if err := rows.Scan(&m, &c); err != nil {
			rows.Close()
			return nil, err
		}
		monthCounts[m.UTC()] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for cursor := windowFrom; !cursor.After(windowTo); cursor = cursor.AddDate(0, 1, 0) {
		stats.Monthly = append(stats.Monthly, CEAMonthlyBin{
			Month: cursor,
			Count: monthCounts[cursor],
		})
	}

	// Top towns — 5 + Other.
	rows, err = s.Pool.Query(ctx, `
		SELECT COALESCE(town, 'OTHER'), COUNT(*)
		  FROM cea_transactions
		 WHERE salesperson_reg_num = $1
		   AND transaction_month >= $2
		   AND transaction_month <  $3
		   AND `+modeExpr+` = $4
		 GROUP BY town
		 ORDER BY COUNT(*) DESC`,
		regNo, windowFrom, windowTo.AddDate(0, 1, 0), mode,
	)
	if err != nil {
		return nil, fmt.Errorf("cea practice stats towns: %w", err)
	}
	const topN = 5
	otherCount := 0
	rank := 0
	for rows.Next() {
		var name string
		var c int
		if err := rows.Scan(&name, &c); err != nil {
			rows.Close()
			return nil, err
		}
		// Fold NULL / unknown towns into the OTHER bucket regardless of
		// where they fall in the ranking — we never want a literal
		// "Other" row competing with real towns for a top-5 slot, and
		// we never want two separate "Other" entries in the final list.
		if name == "OTHER" {
			otherCount += c
			continue
		}
		if rank < topN {
			stats.Towns = append(stats.Towns, CEATownBin{Name: name, Count: c})
		} else {
			otherCount += c
		}
		rank++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if otherCount > 0 {
		stats.Towns = append(stats.Towns, CEATownBin{Name: "OTHER", Count: otherCount})
	}

	// Representation and transaction-type breakdowns — full histograms.
	scanCategory := func(col string) ([]CEACategoryBin, error) {
		rs, err := s.Pool.Query(ctx, `
			SELECT COALESCE(`+col+`, 'OTHER'), COUNT(*)
			  FROM cea_transactions
			 WHERE salesperson_reg_num = $1
			   AND transaction_month >= $2
			   AND transaction_month <  $3
			   AND `+modeExpr+` = $4
			 GROUP BY `+col+`
			 ORDER BY COUNT(*) DESC`,
			regNo, windowFrom, windowTo.AddDate(0, 1, 0), mode,
		)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		out := make([]CEACategoryBin, 0)
		for rs.Next() {
			var name string
			var c int
			if err := rs.Scan(&name, &c); err != nil {
				return nil, err
			}
			out = append(out, CEACategoryBin{Name: name, Count: c})
		}
		return out, rs.Err()
	}
	if stats.Representation, err = scanCategory("represented"); err != nil {
		return nil, fmt.Errorf("cea practice stats rep: %w", err)
	}
	if stats.TxTypes, err = scanCategory("transaction_type"); err != nil {
		return nil, fmt.Errorf("cea practice stats tx_types: %w", err)
	}
	return stats, nil
}

// UpsertCEASalespersons writes the snapshot rows. registration_no is the
// natural key; conflicting rows are updated in place so an agent who has
// switched agencies sees their record refreshed rather than duplicated.
// Returns (rowsIn, rowsAffected) where rowsAffected counts both fresh
// inserts and updates that actually changed something — Postgres reports
// 1 for ON CONFLICT DO UPDATE regardless of whether values changed.
func (s *Store) UpsertCEASalespersons(ctx context.Context, rows []CEASalespersonRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	upserted := 0
	const stmt = `
		INSERT INTO cea_salespersons
		   (registration_no, salesperson_name, registration_start_date,
		    registration_end_date, estate_agent_name, estate_agent_license_no,
		    ingested_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		ON CONFLICT (registration_no) DO UPDATE SET
		   salesperson_name        = EXCLUDED.salesperson_name,
		   registration_start_date = EXCLUDED.registration_start_date,
		   registration_end_date   = EXCLUDED.registration_end_date,
		   estate_agent_name       = EXCLUDED.estate_agent_name,
		   estate_agent_license_no = EXCLUDED.estate_agent_license_no,
		   ingested_at             = now()`
	for _, r := range rows {
		var start, end any
		if !r.RegistrationStartDate.IsZero() {
			start = r.RegistrationStartDate
		}
		if !r.RegistrationEndDate.IsZero() {
			end = r.RegistrationEndDate
		}
		regNo := strings.ToUpper(strings.TrimSpace(r.RegistrationNo))
		if regNo == "" {
			continue
		}
		tag, err := tx.Exec(ctx, stmt,
			regNo, r.SalespersonName, start, end,
			r.EstateAgentName, r.EstateAgentLicenseNo)
		if err != nil {
			return 0, 0, err
		}
		upserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(rows), upserted, nil
}
