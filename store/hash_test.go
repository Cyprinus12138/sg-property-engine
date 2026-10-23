package store

import (
	"encoding/hex"
	"testing"
	"time"
)

// Golden hashes: these bytes are the ON CONFLICT keys for live rows. A
// change here means every previously-ingested row is re-inserted as a
// duplicate on the next sync, so the formula is pinned deliberately.
func TestSourceHashGoldens(t *testing.T) {
	month := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	hdb := HashHDBResale(HDBResaleRow{
		Month: month, Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12",
		FlatType: "4 ROOM", StoreyRange: "07 TO 09", FloorAreaSqm: 93, LeaseCommence: 1987, ResalePrice: 680000,
	})
	assertHex(t, "hdb", hdb, "632cac43fc32c74169266f5ab0f1ace4")

	txn := HashURATxn(URATransactionRow{
		Project: "THE SAIL @ MARINA BAY", Street: "MARINA BOULEVARD", ContractDate: month,
		AreaSqm: 100, Price: 2000000, FloorRange: "41-45", TypeOfSale: "3",
	})
	assertHex(t, "ura_txn", txn, "6bd301eb96597c7a7975aa6a917989eb")

	rent := HashURARental(URARentalRow{
		Project: "X", Street: "Y", LeaseDate: month, AreaSqmMin: 90, AreaSqmMax: 100, Rent: 4500,
	})
	assertHex(t, "ura_rental", rent, "a70d85d9deee49c768fb44337260736b")

	poi := HashPOI(POIRow{Source: "lta_geojson:mrt_exits", Type: "mrt_exit", Name: "BISHAN A", Subtype: "A", Lat: 1.3512, Lng: 103.8485})
	assertHex(t, "poi", poi, "bbed1a184b2da5277b4b3995605283c7")

	cea := HashCEATransaction(CEATransactionRow{
		SalespersonRegNum: "R012345A", SalespersonName: "JANE TAN", TransactionMonth: month,
		PropertyType: "HDB", TransactionType: "RESALE", Represented: "BUYER", Town: "BISHAN", District: "20", GeneralLocation: "",
	})
	assertHex(t, "cea_txn", cea, "c3f65e3b0c2c3d989e20a3aa46674d81")
}

func assertHex(t *testing.T, label string, got []byte, want string) {
	t.Helper()
	if hex.EncodeToString(got) != want {
		t.Errorf("%s hash = %s, want %s", label, hex.EncodeToString(got), want)
	}
}

func TestWalkMinutes(t *testing.T) {
	cases := map[int]int{0: 0, -5: 0, 1: 1, 40: 1, 80: 1, 120: 2, 400: 5, 1000: 13}
	for m, want := range cases {
		if got := walkMinutes(m); got != want {
			t.Errorf("walkMinutes(%d) = %d, want %d", m, got, want)
		}
	}
}

func TestAdvisoryKeyIsStableAndDistinct(t *testing.T) {
	if advisoryKey("hdb") != advisoryKey("hdb") {
		t.Fatal("advisory key must be deterministic")
	}
	if advisoryKey("hdb") == advisoryKey("ura_txn") {
		t.Fatal("different sources must not share a lock key")
	}
}
