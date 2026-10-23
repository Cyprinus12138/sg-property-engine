package sources

// URA Data Service wire-format constants and response schemas.
// Kept in one place so that callers across the codebase share a single
// definition of each envelope rather than redeclaring anonymous structs
// at every decode site.
//
// Docs: https://www.ura.gov.sg/maps/api/

// URA service identifiers — pass to URA.Invoke / InvokeAllBatches /
// InvokeQuarters instead of hardcoding the string.
const (
	ServiceURAResiTransaction   = "PMI_Resi_Transaction"
	ServiceURAResiRental        = "PMI_Resi_Rental"
	ServiceURAResiPipeline      = "PMI_Resi_Pipeline"
	ServiceURAResiDeveloperSale = "PMI_Resi_Developer_Sales"
)

// URATokenResponse is the body of /uraDataService/insertNewToken/v1.
type URATokenResponse struct {
	Status  string `json:"Status"`
	Result  string `json:"Result"`
	Message string `json:"Message"`
}

// URAGenericEnvelope is the common envelope for invokeUraDS responses when
// the caller just wants the Result as a generic slice of maps. Used by the
// raw batch/quarter helpers and by pipeline ingestion.
type URAGenericEnvelope struct {
	Status  string           `json:"Status"`
	Message string           `json:"Message,omitempty"`
	Result  []map[string]any `json:"Result"`
}

// ---- PMI_Resi_Transaction ----
//
// Projects are top-level records with a nested per-transaction array.
// Project-level fields: project, street, marketSegment, x, y.
// (District / tenure / propertyType live on each transaction, not the project.)
// URA is inconsistent about number-vs-string typing across quarters, so
// numeric-ish transaction fields are decoded as `any` and coerced at flatten
// time via asString. PMI_Resi_Transaction does NOT expose noOfBedRoom.

type URAResiTransactionProject struct {
	Project       string                  `json:"project"`
	Street        string                  `json:"street"`
	MarketSegment string                  `json:"marketSegment"`
	X             string                  `json:"x"`
	Y             string                  `json:"y"`
	Transactions  []URAResiTransactionRec `json:"transaction"`
}

type URAResiTransactionRec struct {
	ContractDate any `json:"contractDate"`
	Price        any `json:"price"`
	NettPrice    any `json:"nettPrice"`
	Area         any `json:"area"`
	FloorRange   any `json:"floorRange"`
	NoOfUnits    any `json:"noOfUnits"`
	TypeOfSale   any `json:"typeOfSale"`
	PropertyType any `json:"propertyType"`
	District     any `json:"district"`
	Tenure       any `json:"tenure"`
	TypeOfArea   any `json:"typeOfArea"`
}

type URAResiTransactionEnvelope struct {
	Status  string                      `json:"Status"`
	Message string                      `json:"Message,omitempty"`
	Result  []URAResiTransactionProject `json:"Result"`
}

// ---- PMI_Resi_Rental ----
//
// Project-level fields are only project/street/x/y; propertyType, district,
// noOfBedRoom, rent, areaSqft, areaSqm, leaseDate live per-rental contract.
// areaSqft / areaSqm are RANGE strings like "1500-2000" for privacy.

type URAResiRentalProject struct {
	Project string             `json:"project"`
	Street  string             `json:"street"`
	X       string             `json:"x"`
	Y       string             `json:"y"`
	Rentals []URAResiRentalRec `json:"rental"`
}

type URAResiRentalRec struct {
	LeaseDate    any `json:"leaseDate"`
	PropertyType any `json:"propertyType"`
	District     any `json:"district"`
	NoOfBedroom  any `json:"noOfBedRoom"`
	Rent         any `json:"rent"`
	AreaSqft     any `json:"areaSqft"`
	AreaSqm      any `json:"areaSqm"`
}

type URAResiRentalEnvelope struct {
	Status  string                 `json:"Status"`
	Message string                 `json:"Message,omitempty"`
	Result  []URAResiRentalProject `json:"Result"`
}

// ---- PMI_Resi_Pipeline ----
//
// Flat per-project rows with unit breakdowns by type. Fields:
// project, street, district, developerName, totalUnits, cumUnitsSold,
// status, expectedTOPDate/expectedTOPYear, plus noOfApartment / noOfCondo /
// noOfDetachedHouse / noOfSemiDetached / noOfTerrace. Kept as map[string]any
// because different consumers pick different subsets.

type URAResiPipelineEnvelope = URAGenericEnvelope
