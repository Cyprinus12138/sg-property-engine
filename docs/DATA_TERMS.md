# Data sources and their terms

sg-property-engine ships **no data**. Every row in your database is fetched
from the publisher at run time under *your* registration, and the engine's
licence (PolyForm Noncommercial 1.0.0) covers the code only. Before deploying,
read the current terms at each link below — they change, and this summary is a
pointer, not legal advice.

| Source | Datasets used | Access | Terms |
|---|---|---|---|
| **data.gov.sg** | HDB resale prices; HDB property information; CEA salesperson register; CEA residential transaction records; General information of schools; Train station exits (LTA GeoJSON); Train station codes | No key required. Rate-limited; the engine backs off on 429 and uses the bulk `initiate-download` endpoint for full snapshots. | [Singapore Open Data Licence](https://data.gov.sg/open-data-licence) — attribution required, no implied endorsement. |
| **URA Data Service** | PMI_Resi_Transaction, PMI_Resi_Rental, PMI_Resi_Pipeline, PMI_Resi_Developer_Sales | Free access key: [register](https://eservice.ura.gov.sg/uraDataService/register). Daily token exchange handled by `sources/ura.go`. | [URA Data Service terms](https://www.ura.gov.sg/maps/api/) — personal/organisational use under your own key; check the redistribution clauses before exposing raw rows publicly. |
| **OneMap** | Search (geocoding); Themes API (kindergartens, hawker centres, parks, hospitals, …) | Search is open. Themes need an account: [register](https://www.onemap.gov.sg/apidocs/register). | [OneMap terms of use](https://www.onemap.gov.sg/legal/termsofuse.html) — attribution to OneMap / SLA. |
| **LTA DataMall** | BusStops | Account key: [request](https://datamall.lta.gov.sg/content/datamall/en/request-for-api.html). | [DataMall API terms](https://datamall.lta.gov.sg/content/datamall/en/datamall-apis-and-terms.html) — Singapore Open Data Licence plus DataMall conditions. |
| **Google Maps Platform** (optional) | Places Text Search, as a geocoding fallback only | Billed API key. | [Google Maps Platform terms](https://cloud.google.com/maps-platform/terms) — note the restrictions on caching and storing Places content; the engine stores only lat/lng/postal in `geo_cache`. Leave the key unset if that is a concern. |

## Out of scope by design

- **REALIS** (URA's subscription transaction database) is not used. The URA
  Data Service exposes a rolling window of private transactions; that window is
  what this engine stores.
- **Listings portals** are not crawled. The engine is built on official open
  data only.
- **Personal data.** The CEA datasets are published by the regulator and
  contain salesperson names and registration numbers. Store and serve them in
  line with Singapore's PDPA and the Open Data Licence; the engine exposes them
  only through registration-number lookups and aggregate activity rollups.

## Attribution

When you surface data from this engine, attribute the publisher. A compact
form that satisfies the Open Data Licence:

> Contains information from data.gov.sg, URA Data Service, OneMap and LTA
> DataMall, accessed on *date*, made available under the Singapore Open Data
> Licence v1.0 and the respective providers' terms.
