# sg-property-engine — Design

A standalone Go service that owns Singapore residential property data
acquisition. It ingests from data.gov.sg (HDB, CEA, schools, train station
exits), the URA Data Service (private transactions, rentals, pipeline,
developer sales), OneMap (geocoding and themed POIs) and LTA DataMall (bus
stops), persists everything to Postgres with PostGIS, and exposes a query API
rich enough that downstream services stay thin.

## Goals

1. **Single source of truth.** Downstream code queries this engine's Postgres,
   never the upstream APIs.
2. **Rich query surface.** Filter, sort, paginate, aggregate. The upstream APIs
   expose almost none of this; the engine closes the gap once, in SQL.
3. **First-class geospatial queries.** "MRT within 500 m", "condos within 1 km
   of this block", "schools near project X" are core use cases. Spatial
   filtering happens in SQL via PostGIS so every list endpoint takes the same
   geo filters.
4. **Daily incremental refresh**, run by an internal scheduler. No external
   cron needed.
5. **Explicit backfill.** Fresh databases stay empty until an operator triggers
   a backfill. Boot is always fast, and a wiped `sync_runs` table can never
   fire every backfill at once by accident.
6. **Standalone.** Own binary, own HTTP port, own lifecycle.

## Non-goals

- Real-time streaming. Upstream data updates monthly or quarterly; a daily
  cron is already more frequent than the data changes.
- A generic ETL framework. Each source has a hand-written ingestor.
- Caching in front of the HTTP API. Postgres with good indexes is enough at
  this scale; the Go client memoises for five minutes, which absorbs
  chat-style repeat queries.

## Architecture

```
                 ┌──────────────────────────────────────┐
                 │                 sgpe                 │
                 │                                      │
  data.gov.sg ───┤  ingest/hdb               ┐          │
  URA API     ───┤  ingest/ura_txn           │          │
              ───┤  ingest/ura_rental        │ scheduler│──► Postgres
              ───┤  ingest/ura_pipeline      │ (cron +  │    + PostGIS
              ───┤  ingest/ura_developer_sales│ advisory │
  OneMap      ───┤  ingest/geo (+resolver)   │  locks)  │
  OneMap/LTA  ───┤  ingest/pois              │          │
  data.gov.sg ───┤  ingest/cea, cea_txn      │          │
   (derived)     │  ingest/nearby_mrt        ┘          │
                 │                                      │
                 │  httpapi  /sync/*   (ops)            │
                 │  httpapi  /hdb /ura /projects /pois  │◄── client/ ◄── your service
                 │           /geo /search /explore /cea │
                 └──────────────────────────────────────┘
```

One process, one pgx pool, one HTTP listener (plus an optional Prometheus
listener).

## Layout

```
cmd/sgpe/        main.go (serve | sync | migrate), config.go (env parsing)
sources/         datagov, ura (+schema_ura), onemap, lta, googlemaps — API clients
ingest/          one ingestor per source; all return (rowsIn, rowsUpserted, err)
store/           pgx repositories: hdb, ura, projects, pois, mrt, geo, cea, sync
scheduler/       robfig/cron wiring, advisory locks, progress reporter
httpapi/         query builder (geo filters), handlers, titlecase helper
client/          Go client for the query API
db/              RunMigrations (golang-migrate behind an advisory lock)
migrations/      embedded SQL schema
internal/testdb/ smoke-test bootstrap (template-database cloning)
```

## Schema

All tables come from `migrations/`, applied on boot. PostGIS is a hard
dependency: `000001_init.up.sql` declares `CREATE EXTENSION postgis` ahead of
the spatial tables. Spatial columns are `geography(Point, 4326)` so distance
math is in metres without manual projection; every table with a location has a
GiST index on `geom`.

| Table | Rows | Natural key / idempotency | Notes |
|---|---|---|---|
| `hdb_resale` | one per resale transaction | `source_hash` = md5(month, town, block, street, flat_type, storey, area, lease_commence, price) | no coordinates — joins `hdb_blocks` |
| `hdb_blocks` | one per physical block | PK (street_name, block) | facts from HDB Property Information; geo filled lazily |
| `ura_transactions` | one per private transaction | `source_hash` | `psf` computed at ingest; `contract_date` is month precision |
| `ura_rentals` | one per rental contract | `source_hash` | area kept as a band (`area_sqm_min/max`) |
| `ura_pipeline` | small snapshot | truncate + insert per run | raw JSON kept in `raw` |
| `ura_developer_sales` | one per (project, month) | UNIQUE (project, ref_period) | volatile numbers overwritten on refetch |
| `projects` | one per private project | PK name (UPPERCASE) | derived from `ura_transactions`; geo and `nearby_mrt` survive rebuilds |
| `project_buildings` | one per building | UNIQUE (project, postal) | discovered from OneMap during geocoding |
| `pois` | one per feature | `source_hash` | type-tagged: mrt_exit, lrt_exit, bus_stop, school, kindergartens, … |
| `geo_cache` | one per query string | PK query | geocoder memo incl. negatives |
| `cea_salespersons` | one per salesperson | PK registration_no | current-state snapshot |
| `cea_transactions` | one per attributed deal | `source_hash` over every field | month precision; no price column upstream |
| `sync_runs` | one per job run | — | status, rows, stage, progress, error |

## Incremental strategy

Incrementality is source-by-source; the common guarantee is **idempotent
upserts keyed on `source_hash`**, so any run can safely refetch and overwrite
without creating duplicates.

| Source | Strategy |
|---|---|
| HDB resale | Backfill: bulk CSV download via data.gov.sg `initiate-download`. Daily: fetch current + previous month via `datastore_search` and upsert. Then refresh `hdb_blocks` from HDB Property Information and seed any block seen only in resale. |
| URA transactions | No delta filter exists. Daily: refetch all four batches (rolling window) and upsert; most rows no-op. Then rebuild `projects`. |
| URA rentals | Quarterly ref periods: 2 in incremental mode, 20 in backfill. |
| URA pipeline | Small. Fully refreshed inside one transaction (TRUNCATE + INSERT), then merged into `projects` for pipeline-only launches (placeholder labels skipped, case-twins deduped). |
| URA developer sales | Monthly ref periods: 3 incremental, 36 backfill; upsert on (project, month). |
| Geo | Lazy: geocodes `hdb_blocks` and `projects` with `geom IS NULL` only. OneMap first, Google Places fallback, results memoised in `geo_cache`. Discovers `project_buildings` from the same OneMap response. |
| POIs | Per-source sync: upsert then prune rows that vanished upstream, in one transaction. An empty upstream response is treated as a failure so a blip never wipes a table. MRT exits are enriched with station codes/lines from the station-codes CSV plus a small supplemental list for lines newer than the CSV. |
| CEA | Full-snapshot CSVs; idempotent by `source_hash` / PK. |
| nearby_mrt | Pure SQL enrichment: for each project with coordinates and stale `nearby_mrt_synced_at`, the union of the top 3 stations and every station within 1 km, one row per station (closest exit wins). Backfill mode forces every project. |

## Scheduler

`github.com/robfig/cron/v3` inside the process. Defaults (server-local time,
override via `SGPE_CRON_*`):

| Job | Cron | Depends on |
|---|---|---|
| `hdb` | `0 2 * * *` | — |
| `cea` | `30 2 * * *` | — |
| `cea_txn` | `45 2 * * *` | cea (for registration numbers) |
| `ura_txn` | `0 3 * * *` | — |
| `ura_rental` | `30 3 * * *` | — |
| `ura_pipeline` | `45 3 * * *` | ura_txn (projects rebuild) |
| `ura_developer_sales` | `0 4 * * *` | — |
| `geo` | `15 4 * * *` | hdb, ura_txn |
| `pois` | `0 5 * * 1` | — (weekly) |
| `nearby_mrt` | `0 6 * * *` | pois, geo |

Every job takes a **Postgres advisory lock** keyed on its source name before
running, so two engine processes against the same database cannot collide and
a manual `/sync/run` cannot overlap a cron tick. A second trigger while a
source is running returns `409`. A run that loses the lock race is recorded as
`skipped`, not `error`. Each run writes a `sync_runs` row on start and
finalises it on completion; the progress reporter throttles writes to one per
two seconds except on stage changes and the terminal 100%.

`sgpe sync --source X --mode backfill` runs one job in the foreground through
the same scheduler (cron disarmed) and exits non-zero if the run did not end
in `success`, so it drops straight into an external cron or a one-shot
container.

## Geo filter conventions

Spatial queries come in two shapes, supported uniformly on every list endpoint
whose rows have a location.

**Near this anchor** — rows within N metres of a point, implemented as
`ST_DWithin(row.geom, anchor, radius_m)`. The anchor is resolved server-side
from `near_lat/near_lng`, `near_project`, `near_poi_type/near_poi_name` or
`near_block/near_street`. Radius defaults to 1000 m, capped at 5000 m. When an
anchor is present every row carries `distance_m` and `sort=distance_asc` is
available.

**Has something nearby** — rows with at least one POI of a type within N
metres *of that row*, a spatial self-join:

```sql
... WHERE EXISTS (
  SELECT 1 FROM pois p
  WHERE p.type = :poi_type
    AND ST_DWithin(row.geom, p.geom, :within_m)
)
```

The two compose: "projects within 2 km of Orchard MRT that also have a primary
school within 500 m" is one request. All filters are built by one query
builder (`httpapi/query.go`) so every endpoint exposes an identical geo
vocabulary.

## Operational notes

- **Display case.** URA emits names and streets in UPPERCASE and the database
  keeps them that way so joins stay stable. Responses add a Title-Cased
  `display_name` (acronyms such as EC, MRT and Roman numerals preserved).
- **Anchoring on data, not on NOW().** `/explore/*` windows are anchored on the
  most recent `contract_date` in the table because URA reporting lags by weeks;
  anchoring on the wall clock would routinely return zero rows.
- **Placeholder zeros.** URA occasionally emits `psf = 0` for bulk deals;
  aggregations filter `psf > 0`.
- **Rate limits.** data.gov.sg returns 429 under load; the client waits and
  retries, and the bulk download endpoint is preferred for full snapshots.
- **Metrics.** Set `SGPE_METRICS_ADDR=:2112` to expose Prometheus metrics on a
  separate listener.
