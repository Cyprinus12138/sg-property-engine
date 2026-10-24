package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// opsTable is one table the ops endpoints may describe and preview. The
// list is the allowlist: nothing outside it is ever interpolated into SQL.
type opsTable struct {
	name    string
	timeCol string // MAX() of this column is the table's "last update"
	orderBy string // stable preview order, newest-first where it matters
}

var opsTables = []opsTable{
	{"hdb_resale", "ingested_at", "month DESC, id DESC"},
	{"hdb_blocks", "geocoded_at", "street_name, block"},
	{"ura_transactions", "ingested_at", "contract_date DESC, id DESC"},
	{"ura_rentals", "ingested_at", "lease_date DESC, id DESC"},
	{"ura_pipeline", "ingested_at", "id"},
	{"ura_developer_sales", "ingested_at", "ref_period DESC, project"},
	{"projects", "updated_at", "name"},
	{"project_buildings", "discovered_at", "project, id"},
	{"pois", "ingested_at", "type, name, id"},
	{"geo_cache", "fetched_at", "fetched_at DESC"},
	{"cea_salespersons", "ingested_at", "registration_no"},
	{"cea_transactions", "ingested_at", "transaction_month DESC, id DESC"},
	{"sync_runs", "started_at", "id DESC"},
}

func lookupOpsTable(name string) (opsTable, bool) {
	for _, t := range opsTables {
		if t.name == name {
			return t, true
		}
	}
	return opsTable{}, false
}

// ErrUnknownTable is returned by PreviewRows for a table outside the
// allowlist.
var ErrUnknownTable = errors.New("unknown table")

// TableStat is one row of the ops table overview.
type TableStat struct {
	Table      string     `json:"table"`
	RowCount   int64      `json:"row_count"`
	SizeBytes  int64      `json:"size_bytes"`
	LastUpdate *time.Time `json:"last_update"`
}

// TableStats returns row count, on-disk size (incl. indexes and TOAST) and
// last write time for every ops table. Counts are exact; at Singapore-scale
// row counts that is well under a second for the whole list.
func (s *Store) TableStats(ctx context.Context) ([]TableStat, error) {
	out := make([]TableStat, 0, len(opsTables))
	for _, t := range opsTables {
		ts := TableStat{Table: t.name}
		ident := pgx.Identifier{t.name}.Sanitize()
		q := fmt.Sprintf(`SELECT COUNT(*), pg_total_relation_size($1::regclass), MAX(%s) FROM %s`,
			pgx.Identifier{t.timeCol}.Sanitize(), ident)
		if err := s.Pool.QueryRow(ctx, q, t.name).Scan(&ts.RowCount, &ts.SizeBytes, &ts.LastUpdate); err != nil {
			return nil, fmt.Errorf("stats for %s: %w", t.name, err)
		}
		out = append(out, ts)
	}
	return out, nil
}

// TablePreview is a page of raw rows from one ops table. Columns keeps the
// schema order, which a JSON object would lose.
type TablePreview struct {
	Table   string           `json:"table"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int64            `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// PreviewRows returns limit rows of table starting at offset. Geography
// columns come back as WKT and bytea as hex so the result is readable JSON.
// The caller bounds limit and offset.
func (s *Store) PreviewRows(ctx context.Context, table string, limit, offset int) (*TablePreview, error) {
	t, ok := lookupOpsTable(table)
	if !ok {
		return nil, ErrUnknownTable
	}
	cols, err := s.Pool.Query(ctx, `
		SELECT column_name, udt_name
		  FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = $1
		 ORDER BY ordinal_position`, t.name)
	if err != nil {
		return nil, err
	}
	var names, exprs []string
	for cols.Next() {
		var name, udt string
		if err := cols.Scan(&name, &udt); err != nil {
			cols.Close()
			return nil, err
		}
		ident := pgx.Identifier{name}.Sanitize()
		switch udt {
		case "geography", "geometry":
			exprs = append(exprs, fmt.Sprintf("ST_AsText(%s) AS %s", ident, ident))
		case "bytea":
			exprs = append(exprs, fmt.Sprintf("encode(%s, 'hex') AS %s", ident, ident))
		default:
			exprs = append(exprs, ident)
		}
		names = append(names, name)
	}
	cols.Close()
	if err := cols.Err(); err != nil {
		return nil, err
	}

	p := &TablePreview{Table: t.name, Columns: names, Rows: []map[string]any{}, Limit: limit, Offset: offset}
	ident := pgx.Identifier{t.name}.Sanitize()
	if err := s.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+ident).Scan(&p.Total); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT $1 OFFSET $2",
		strings.Join(exprs, ", "), ident, t.orderBy), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(names))
		for i, n := range names {
			row[n] = vals[i]
		}
		p.Rows = append(p.Rows, row)
	}
	return p, rows.Err()
}

// ListRuns returns sync_runs newest first, optionally for one source, plus
// the total matching count.
func (s *Store) ListRuns(ctx context.Context, source string, limit, offset int) ([]SyncRun, int64, error) {
	var total int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM sync_runs WHERE $1 = '' OR source = $1`, source).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, source, mode, started_at, finished_at, status,
		       rows_in, rows_upserted, COALESCE(stage, ''),
		       COALESCE(progress, 0), COALESCE(error, '')
		  FROM sync_runs
		 WHERE $1 = '' OR source = $1
		 ORDER BY started_at DESC, id DESC
		 LIMIT $2 OFFSET $3`, source, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SyncRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// OpsTableNames lists the tables TableStats and PreviewRows cover.
func OpsTableNames() []string {
	out := make([]string, len(opsTables))
	for i, t := range opsTables {
		out[i] = t.name
	}
	return out
}
