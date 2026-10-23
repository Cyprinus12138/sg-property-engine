package sources

// Paging defaults for the upstream APIs. Kept here rather than at the
// call sites so a provider changing its page cap is a one-line edit.
const (
	// dataGovBatchDefault is the CKAN datastore_search page size used when
	// a caller passes limit <= 0.
	dataGovBatchDefault = 100

	// ltaBusStopPageSize is LTA DataMall's fixed OData page size; the API
	// returns at most 500 rows per call and paginates via $skip.
	ltaBusStopPageSize = 500
)
