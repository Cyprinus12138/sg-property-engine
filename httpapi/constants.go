package httpapi

// Paging bounds for the query API.
const (
	// defaultPageSize / maxPageSize apply to every list endpoint that
	// returns {rows, total, limit, offset}.
	defaultPageSize = 50
	maxPageSize     = 500

	// searchPageDefault / searchPageMax bound /search/properties, which
	// backs an autocomplete dropdown and never needs deep paging.
	searchPageDefault = 10
	searchPageMax     = 50

	// hottestPageDefault / hottestPageMax bound /explore/hottest.
	hottestPageDefault = 15
	hottestPageMax     = 50
)
