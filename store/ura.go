package store

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"time"
)

type URATransactionRow struct {
	Project       string    `json:"project"`
	Street        string    `json:"street"`
	District      string    `json:"district"`
	PropertyType  string    `json:"property_type"`
	Tenure        string    `json:"tenure"`
	MarketSegment string    `json:"market_segment"`
	AreaSqm       float64   `json:"area_sqm"`
	Price         float64   `json:"price"`
	PSF           float64   `json:"psf"`
	ContractDate  time.Time `json:"contract_date"`
	FloorRange    string    `json:"floor_range"`
	TypeOfSale    string    `json:"type_of_sale"`
}

func HashURATxn(r URATransactionRow) []byte {
	h := md5.New()
	fmt.Fprintf(h, "txn|%s|%s|%s|%.2f|%.2f|%s|%s",
		r.Project, r.Street, r.ContractDate.Format("2006-01-02"),
		r.AreaSqm, r.Price, r.FloorRange, r.TypeOfSale)
	return h.Sum(nil)
}

func (s *Store) UpsertURATransactions(ctx context.Context, rows []URATransactionRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	const stmt = `
		INSERT INTO ura_transactions
		   (project, street, district, property_type, tenure, market_segment,
		    area_sqm, price, psf, contract_date, floor_range, type_of_sale,
		    source_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (source_hash) DO NOTHING`
	upserted := 0
	for _, r := range rows {
		tag, err := tx.Exec(ctx, stmt,
			r.Project, r.Street, r.District, r.PropertyType, r.Tenure, r.MarketSegment,
			r.AreaSqm, r.Price, r.PSF, r.ContractDate, r.FloorRange, r.TypeOfSale,
			HashURATxn(r))
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

type URARentalRow struct {
	Project      string `json:"project"`
	Street       string `json:"street"`
	District     string `json:"district"`
	PropertyType string `json:"property_type"`
	// URA publishes rental floor area as a band (e.g. "900-1000" sqm) for
	// privacy, so we persist both edges rather than a single number.
	AreaSqmMin float64   `json:"area_sqm_min"`
	AreaSqmMax float64   `json:"area_sqm_max"`
	Rent       float64   `json:"rent"`
	LeaseDate  time.Time `json:"lease_date"`
	BedroomNum float64   `json:"bedroom_num"`
}

func HashURARental(r URARentalRow) []byte {
	h := md5.New()
	fmt.Fprintf(h, "rent|%s|%s|%s|%.2f|%.2f|%.2f",
		r.Project, r.Street, r.LeaseDate.Format("2006-01-02"),
		r.AreaSqmMin, r.AreaSqmMax, r.Rent)
	return h.Sum(nil)
}

func (s *Store) UpsertURARentals(ctx context.Context, rows []URARentalRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	const stmt = `
		INSERT INTO ura_rentals
		   (project, street, district, property_type,
		    area_sqm_min, area_sqm_max, rent,
		    lease_date, bedroom_num, source_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source_hash) DO NOTHING`
	upserted := 0
	for _, r := range rows {
		tag, err := tx.Exec(ctx, stmt,
			r.Project, r.Street, r.District, r.PropertyType,
			r.AreaSqmMin, r.AreaSqmMax, r.Rent,
			r.LeaseDate, r.BedroomNum, HashURARental(r))
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

type URAPipelineRow struct {
	Project       string
	Developer     string
	Street        string
	District      string
	MarketSegment string
	TotalUnits    int
	UnitsSold     int
	ExpectedTop   string
	Status        string
	Raw           map[string]any
}

type URADeveloperSalesRow struct {
	Project         string    `json:"project"`
	Developer       string    `json:"developer"`
	Street          string    `json:"street"`
	District        string    `json:"district"`
	MarketSegment   string    `json:"market_segment"`
	PropertyType    string    `json:"property_type"`
	RefPeriod       time.Time `json:"ref_period"`
	UnitsAvail      int       `json:"units_avail"`
	LaunchedToDate  int       `json:"launched_to_date"`
	SoldToDate      int       `json:"sold_to_date"`
	LaunchedInMonth int       `json:"launched_in_month"`
	SoldInMonth     int       `json:"sold_in_month"`
	LowestPrice     float64   `json:"lowest_price"`
	MedianPrice     float64   `json:"median_price"`
	HighestPrice    float64   `json:"highest_price"`
}

// UpsertURADeveloperSales upserts monthly developer-sales snapshots keyed on
// (project, ref_period). Re-fetching the same month overwrites volatile
// numbers (sold/launched/prices) — URA may revise an old month's median PSF
// once more transactions land in the next refresh cycle.
func (s *Store) UpsertURADeveloperSales(ctx context.Context, rows []URADeveloperSalesRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	const stmt = `
		INSERT INTO ura_developer_sales
		   (project, developer, street, district, market_segment, property_type,
		    ref_period, units_avail, launched_to_date, sold_to_date,
		    launched_in_month, sold_in_month,
		    lowest_price, median_price, highest_price)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (project, ref_period) DO UPDATE SET
		    developer         = COALESCE(NULLIF(EXCLUDED.developer,''),       ura_developer_sales.developer),
		    street            = COALESCE(NULLIF(EXCLUDED.street,''),          ura_developer_sales.street),
		    district          = COALESCE(NULLIF(EXCLUDED.district,''),        ura_developer_sales.district),
		    market_segment    = COALESCE(NULLIF(EXCLUDED.market_segment,''),  ura_developer_sales.market_segment),
		    property_type     = COALESCE(NULLIF(EXCLUDED.property_type,''),   ura_developer_sales.property_type),
		    units_avail       = EXCLUDED.units_avail,
		    launched_to_date  = EXCLUDED.launched_to_date,
		    sold_to_date      = EXCLUDED.sold_to_date,
		    launched_in_month = EXCLUDED.launched_in_month,
		    sold_in_month     = EXCLUDED.sold_in_month,
		    lowest_price      = EXCLUDED.lowest_price,
		    median_price      = EXCLUDED.median_price,
		    highest_price     = EXCLUDED.highest_price,
		    ingested_at       = now()`
	upserted := 0
	for _, r := range rows {
		tag, err := tx.Exec(ctx, stmt,
			r.Project, r.Developer, r.Street, r.District, r.MarketSegment, r.PropertyType,
			r.RefPeriod, r.UnitsAvail, r.LaunchedToDate, r.SoldToDate,
			r.LaunchedInMonth, r.SoldInMonth,
			r.LowestPrice, r.MedianPrice, r.HighestPrice)
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

// RefreshURAPipeline truncates and re-inserts the pipeline table in one
// transaction — pipeline data is a small snapshot.
func (s *Store) RefreshURAPipeline(ctx context.Context, rows []URAPipelineRow) (int, int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "TRUNCATE ura_pipeline"); err != nil {
		return 0, 0, err
	}
	const stmt = `
		INSERT INTO ura_pipeline
		  (project, developer, street, district, market_segment,
		   total_units, units_sold, expected_top, status, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	upserted := 0
	for _, r := range rows {
		raw, _ := json.Marshal(r.Raw)
		tag, err := tx.Exec(ctx, stmt,
			r.Project, r.Developer, r.Street, r.District, r.MarketSegment,
			r.TotalUnits, r.UnitsSold, r.ExpectedTop, r.Status, raw)
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
