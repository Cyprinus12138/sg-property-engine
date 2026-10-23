# sg-property-engine

[![CI](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Cyprinus12138/sg-property-engine)](https://goreportcard.com/report/github.com/Cyprinus12138/sg-property-engine)
[![License: PolyForm Noncommercial 1.0.0](https://img.shields.io/badge/license-PolyForm%20Noncommercial%201.0.0-blue)](LICENSE)

Singapore residential property open data, ingested on a schedule into Postgres +
PostGIS and served through one geo-aware query API.

Extracted from [ProProp](https://proprop.algonavis.com/), an AI property advisor
for Singapore running in production, where this engine feeds the agent's market,
project and geo tools from a nightly-refreshed PostGIS store.

## What it does

- **Ingests the official sources** — HDB resale transactions and block facts
  (data.gov.sg), URA private transactions, rentals, launch pipeline and
  developer sales (URA Data Service), MRT/LRT exits, schools, bus stops and
  OneMap themed POIs (data.gov.sg, OneMap, LTA DataMall), and the CEA
  salesperson register and transaction records.
- **Idempotent by construction** — every row carries a `source_hash` of its
  natural key, so a daily refetch is a no-op for rows already present. Upstream
  has no "changed since" filter; this is the honest ceiling.
- **Derives what the sources don't give you** — a `projects` master built from
  URA transactions (and pipeline-only new launches), lazily geocoded through
  OneMap with a Google Places fallback, plus per-project `nearby_mrt` (nearest
  stations with line codes, distance and walking minutes).
- **Answers spatial questions in SQL** — every list endpoint takes the same geo
  filters: *near this anchor* (`ST_DWithin` against a point, project, POI or
  HDB block) and *has a POI within N metres* (a spatial `EXISTS`). They compose:
  "condos in District 15 with an MRT exit within 500 m" is one request.
- **Runs itself** — an in-process cron scheduler with per-source Postgres
  advisory locks (two replicas cannot run the same job; a manual trigger and a
  cron tick cannot collide), `sync_runs` bookkeeping with throttled progress
  reporting, and a `409` when a source is already running.

## Quickstart

```bash
cp .env.example .env          # add URA_ACCESS_KEY etc. — all optional, see below
docker compose up --build     # PostGIS + engine on :8081
```

The database starts empty on purpose: boot is fast and nothing fires until you
ask. Kick off the first HDB backfill and watch it:

```bash
curl -X POST 'http://localhost:8081/sync/run?source=hdb&mode=backfill'
curl        'http://localhost:8081/sync/status'
```

Then query:

```bash
# HDB resale within 800 m of Bishan MRT, nearest first
curl 'http://localhost:8081/hdb/transactions?near_poi_type=mrt_exit&near_poi_name=BISHAN&radius_m=800&sort=distance_asc'

# condos in D15 that have an MRT exit within 500 m
curl 'http://localhost:8081/projects?district=15&property_type=Condominium&has_poi_type=mrt_exit&has_poi_within_m=500'

# median PSF by town, last 12 months
curl 'http://localhost:8081/hdb/stats?group_by=town&metric=median_psf&min_month=2025-10-01'

# nearest stations to an arbitrary point
curl 'http://localhost:8081/geo/nearest-stations?lat=1.3521&lng=103.8198'
```

Without Docker:

```bash
createdb sgpe && psql sgpe -c 'CREATE EXTENSION postgis;'
SGPE_DB_URL=postgres://localhost/sgpe?sslmode=disable go run ./cmd/sgpe serve
# or run one job in the foreground and exit:
SGPE_DB_URL=... go run ./cmd/sgpe sync --source hdb --mode backfill
```

Daily incremental syncs are scheduled automatically (see the cron table in
[docs/DATA_ENGINE.md](docs/DATA_ENGINE.md)). Set `SGPE_BOOTSTRAP_ON_BOOT=true`
if you *do* want a fresh deployment to backfill every source at startup.

### Credentials

All upstream keys are optional; a missing key disables that source and the
startup log says so. See [.env.example](.env.example) for where to register.

| Variable | Enables |
|---|---|
| `URA_ACCESS_KEY` | `ura_txn`, `ura_rental`, `ura_pipeline`, `ura_developer_sales` |
| `ONEMAP_EMAIL` / `ONEMAP_PASSWORD` | OneMap Themes POIs (kindergartens, hawker centres, parks, hospitals). Geocoding search works without them. |
| `LTA_ACCOUNT_KEY` | Bus stops |
| `GOOGLE_MAPS_API_KEY` | Geocoding fallback when OneMap has no result |

No data is bundled with this repository and none is redistributed — the engine
pulls from the publishers under your own registration. Read
[docs/DATA_TERMS.md](docs/DATA_TERMS.md) before deploying.

## API

Single listener, JSON in and out. List endpoints return
`{rows, total, limit, offset}`; stats endpoints return `{buckets: [{key, value, count}]}`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | DB ping |
| GET | `/sync/status` | latest run per source + known sources |
| POST | `/sync/run?source=&mode=incremental\|backfill[&arg=]` | trigger a job (202 with `run_id`; 409 if running) |
| GET | `/sync/runs/{id}` | one `sync_runs` row with stage/progress |
| GET | `/hdb/transactions` | filters: town, flat_type, block, street, min/max_month, min/max_price, min/max_area, storey, sort + geo |
| GET | `/hdb/stats` | `group_by=month\|town\|flat_type`, `metric=median_psf\|median_price\|avg_price\|count` + the same filters |
| GET | `/ura/transactions` | filters: project, district, property_type, tenure, market_segment, min/max_date, min/max_price, min/max_psf + geo |
| GET | `/ura/rentals` | filters: project, district, property_type, min/max_date + geo |
| GET | `/ura/rentals/trend` | monthly P25 / median / P75 rent; filters: district, property_type, bedroom_num, min/max_date |
| GET | `/ura/pipeline` | upcoming / under-construction projects; filters: district, status |
| GET | `/ura/developer-sales` | monthly developer-sales snapshots; filters: project, district, market_segment, developer, min/max_period |
| GET | `/projects` | filters: name (prefix), district, property_type + geo |
| GET | `/projects/{name}` | one project (case/punctuation-insensitive) incl. `nearby_mrt` |
| GET | `/pois` | filters: type, subtype, name (prefix) + near-anchor geo |
| GET | `/pois/types` | ingested POI types with counts |
| GET | `/geo/nearest-stations?lat=&lng=[&limit=][&within_m=]` | nearest MRT/LRT stations for a point |
| GET | `/search/properties?q=` | autocomplete across projects and HDB blocks |
| GET | `/explore/hottest` | projects ranked by trailing-90-day URA volume with PSF trend |
| GET | `/explore/district_median?district=` | median price / PSF for a district, optional area band |
| GET | `/cea/lookup?registration_no=` | CEA salesperson registration lookup |
| GET | `/cea/practice-stats?reg_no=&mode=sale\|rent` | 12-month rolling activity rollup for one salesperson |

**Geo filters** (every list endpoint whose rows have a location):

| Param | Meaning |
|---|---|
| `near_lat` + `near_lng` | anchor at raw coordinates |
| `near_project` | anchor at a project centroid |
| `near_poi_type` + `near_poi_name` | anchor at a named POI, e.g. `mrt_exit` + `BISHAN` |
| `near_block` + `near_street` | anchor at an HDB block |
| `radius_m` | default 1000, max 5000; rows gain `distance_m`; `sort=distance_asc` |
| `has_poi_type` + `has_poi_within_m` [+ `has_poi_subtype`] | keep rows with a matching POI within range (default 500 m) |

URA publishes names in UPPERCASE; responses keep the raw `name` as the lookup
key and add a Title-Cased `display_name`.

### Go client

```go
import "github.com/Cyprinus12138/sg-property-engine/client"

c := client.New("http://localhost:8081")
page, err := c.HDBTransactions(ctx, url.Values{"town": {"BISHAN"}, "limit": {"20"}})
proj, err := c.Project(ctx, "The Sail @ Marina Bay")   // proj.NearbyMRT …
```

Responses are cached in-process for five minutes (configurable); 404s map to
`client.ErrNotFound`.

## Architecture

```
  data.gov.sg ──┐  ingest/hdb, cea, cea_transactions, pois(schools, mrt exits)
  URA API     ──┤  ingest/ura_txn, ura_rental, ura_pipeline, ura_developer_sales
  OneMap      ──┤  ingest/geo (+ resolver), pois (themes)          scheduler
  LTA DataMall──┤  ingest/pois (bus stops)                      ┌──(cron + advisory locks)
  Google Maps ──┘  ingest/resolver (fallback only)              │
                          │                                     │
                          ▼                                     ▼
                   store/ (pgx)  ───────────────────────►  Postgres + PostGIS
                                                                 ▲
                   httpapi/ (query + ops)  ◄── client/ ──── your service
```

Design notes, schema and the incremental strategy per source live in
[docs/DATA_ENGINE.md](docs/DATA_ENGINE.md).

## Development

```bash
go test ./...                                   # unit tests, no services
docker run --rm -d --name sgpe-pg -e POSTGRES_PASSWORD=test \
  -p 55433:5432 postgis/postgis:16-3.4
TEST_DATABASE_URL=postgres://postgres:test@localhost:55433/postgres?sslmode=disable \
  go test -tags=smoke -count=1 -p 1 ./...       # end-to-end over a real PostGIS
```

Smoke tests clone a migrated template database per test, so they are isolated
and fast after the first run. Schema changes are migrations under
`migrations/` — never boot-time `ALTER`s.

## License

[PolyForm Noncommercial 1.0.0](LICENSE). Noncommercial use — personal,
research, educational, and by noncommercial organisations — is free. Commercial
use requires a separate licence from the author; open an issue or contact
[Mr.Cyprinus](https://github.com/Cyprinus12138). Attribution is required: keep
the `Required Notice` line from the LICENSE and the NOTICE file with any copy.

The licence covers this code only. Data fetched by the engine remains subject
to each publisher's terms — see [docs/DATA_TERMS.md](docs/DATA_TERMS.md).
