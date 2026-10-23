package ingest

// Batch and page sizes for the ingestors. Each value trades upstream
// request count against transaction size; none are tuned for throughput —
// these are background jobs and the upstream rate limits dominate.
const (
	// hdbPropertyInfoPageSize pages the HDB Property Information dataset
	// (~13.3k rows) through CKAN datastore_search.
	hdbPropertyInfoPageSize = 5000
	// hdbResaleBatchSize is the per-transaction upsert batch for the HDB
	// resale CSV backfill and the CEA transaction CSV.
	hdbResaleBatchSize = 5000
	// hdbResalePageSize pages a single month of HDB resale rows in
	// incremental mode.
	hdbResalePageSize = 10000
	// geoIngestBatchSize caps blocks/projects geocoded per batch.
	geoIngestBatchSize = 500
	// ceaSalespersonBatchSize is the per-transaction upsert batch for the
	// CEA salesperson register CSV.
	ceaSalespersonBatchSize = 2000
)
