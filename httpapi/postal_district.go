package httpapi

import "strings"

// postalToDistrict maps a 6-digit Singapore postal code to its 2-digit
// postal district ("01"..."28") per URA's "List of Postal Districts"
// (source: SingPost; https://www.ura.gov.sg/Corporate/-/media/Corporate/
// Property/PMI-Online/List_Of_Postal_Districts.pdf).
//
// Districts are derived from the first 2 digits of the postal code (the
// postal sector). Returns "" for empty input, sentinels like "NI", or
// any sector not in the URA table.
//
// The mapping is stable since the 1990s; if URA ever amends it the
// table here is the single source of truth for the data engine.
func postalToDistrict(postal string) string {
	postal = strings.TrimSpace(postal)
	if len(postal) < 2 {
		return ""
	}
	sector := postal[:2]
	for _, c := range sector {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return postalSectorDistrict[sector]
}

// postalSectorDistrict is the URA postal-sector → district lookup.
// Sectors not listed (e.g. 74) are intentionally absent and resolve to
// the empty string, which is treated as "unknown" upstream.
var postalSectorDistrict = map[string]string{
	"01": "01", "02": "01", "03": "01", "04": "01", "05": "01", "06": "01",
	"07": "02", "08": "02",
	"14": "03", "15": "03", "16": "03",
	"09": "04", "10": "04",
	"11": "05", "12": "05", "13": "05",
	"17": "06",
	"18": "07", "19": "07",
	"20": "08", "21": "08",
	"22": "09", "23": "09",
	"24": "10", "25": "10", "26": "10", "27": "10",
	"28": "11", "29": "11", "30": "11",
	"31": "12", "32": "12", "33": "12",
	"34": "13", "35": "13", "36": "13", "37": "13",
	"38": "14", "39": "14", "40": "14", "41": "14",
	"42": "15", "43": "15", "44": "15", "45": "15",
	"46": "16", "47": "16", "48": "16",
	"49": "17", "50": "17", "81": "17",
	"51": "18", "52": "18",
	"53": "19", "54": "19", "55": "19", "82": "19",
	"56": "20", "57": "20",
	"58": "21", "59": "21",
	"60": "22", "61": "22", "62": "22", "63": "22", "64": "22",
	"65": "23", "66": "23", "67": "23", "68": "23",
	"69": "24", "70": "24", "71": "24",
	"72": "25", "73": "25",
	"77": "26", "78": "26",
	"75": "27", "76": "27",
	"79": "28", "80": "28",
}
