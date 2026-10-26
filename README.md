# sg-property-engine

**English** · [简体中文](README.zh-CN.md)

[![CI](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Cyprinus12138/sg-property-engine.svg)](https://pkg.go.dev/github.com/Cyprinus12138/sg-property-engine)
[![Go Report Card](https://goreportcard.com/badge/github.com/Cyprinus12138/sg-property-engine)](https://goreportcard.com/report/github.com/Cyprinus12138/sg-property-engine)
[![Go version](https://img.shields.io/github/go-mod/go-version/Cyprinus12138/sg-property-engine)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

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
- **Idempotent ingestion** — transaction and POI rows are deduplicated by
  `source_hash`; POI syncs upsert rows and remove those no longer present
  upstream. Other tables use natural keys or transactional snapshot replacement.
  Refetching data does not create duplicates. Where an upstream API has no
  "changed since" filter, the engine refetches the available window.
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
- **Speaks MCP** — the same queries as read-only Model Context Protocol tools,
  over HTTP at `/mcp` or over stdio with `sgpe mcp`, so an AI assistant can
  answer "4-room resale prices near Bishan MRT" from the database directly.
- **Ships an ops page** — `/ui/` in the same binary shows each source's status
  and progress, starts incremental or backfill runs, lists run history, and
  previews any table. No Node toolchain; three static files are embedded.

## Quickstart

One command, three ways to run it. Each waits until `/healthz` answers.

```bash
scripts/stack.sh up                 # docker compose: PostGIS + engine image (default)
scripts/stack.sh up docker          # plain `docker run`, no compose needed
scripts/stack.sh up local           # Go binary on the host (Go 1.25+)
```

`make up` / `make up MODE=local` / `make dev` (local, foreground) wrap the same
script. `scripts/stack.sh status`, `logs` and `down` (`down --purge` also deletes
the data) work for every mode.

The database starts empty, and startup does not trigger a backfill by default.
Scheduled incremental syncs still run. Start the first HDB backfill and
watch its progress:

```bash
scripts/stack.sh up --no-build --backfill hdb      # or: curl -X POST 'http://localhost:8081/sync/run?source=hdb&mode=backfill'
curl 'http://localhost:8081/sync/status'
```

Or open <http://localhost:8081/ui/> (`/` redirects there) to watch progress,
start runs and browse the tables.

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

Or drive the binary directly:

```bash
go run ./cmd/sgpe serve                                   # no DB configured → starts a local PostGIS via Docker
go run ./cmd/sgpe sync --source hdb --mode backfill       # one job in the foreground, then exit
go run ./cmd/sgpe --config prod.yaml migrate
go run ./cmd/sgpe config                                  # print the effective config, secrets masked
```

Daily incremental syncs are scheduled automatically (see the cron table in
[docs/DATA_ENGINE.md](docs/DATA_ENGINE.md)). Set `server.bootstrap_on_boot: true`
if you *do* want a fresh deployment to backfill every source at startup.

## Operating the engine

`sgpe serve` hosts an ops page at `/ui/`; here on a local database with the HDB, MRT and POI sources synced:

<table>
<tr><td width="50%" valign="top"><a href="docs/images/ops-jobs.png"><img src="docs/images/ops-jobs.png" width="430" alt="Jobs: one row per source with manual triggers"></a><br><sub><b>Jobs.</b> Status, stage and last run per source, with incremental and backfill triggers.</sub></td><td width="50%" valign="top"><a href="docs/images/ops-tables.png"><img src="docs/images/ops-tables.png" width="430" alt="Tables with a raw-row preview"></a><br><sub><b>Tables.</b> Row count, size and last write per table, and a raw-row preview.</sub></td></tr>
</table>

The original data engine running at production scale in ProProp's operations console:

<table>
<tr><td width="33%" valign="top"><a href="docs/images/proprop-jobs.png"><img src="docs/images/proprop-jobs.png" width="270" alt="Jobs in ProProp's console"></a><br><sub><b>Jobs</b>, one row per ingest source.</sub></td><td width="33%" valign="top"><a href="docs/images/proprop-runs.png"><img src="docs/images/proprop-runs.png" width="270" alt="Run history"></a><br><sub><b>Run history</b> with mode, duration and row counts.</sub></td><td width="33%" valign="top"><a href="docs/images/proprop-tables.png"><img src="docs/images/proprop-tables.png" width="270" alt="Tables at production scale"></a><br><sub><b>Tables:</b> 243k HDB resale rows, 456k URA rentals, 1.2M CEA transactions.</sub></td></tr>
</table>

## Configuration

Three layers, later ones winning:

1. built-in defaults;
2. a YAML file: `--config FILE`, else `$SGPE_CONFIG`, else `./sgpe.yaml` if it
   exists. Copy [sgpe.example.yaml](sgpe.example.yaml), which lists every key
   with its default and its environment variable. Unknown keys are an error, so
   a typo fails at startup instead of quietly disabling a source;
3. environment variables, including a `.env` file
   ([.env.example](.env.example)). Empty values count as unset.

**No database configured?** `serve`, `sync` and `migrate` then start a local
PostGIS container through the Docker CLI, or reuse one, and connect to it.
The container is called `sgpe-postgis`, binds to `127.0.0.1:55432` and keeps its
data in the `sgpe-pgdata` volume, so a backfill survives restarts. This is a
development convenience, not a deployment. Set `database.url` (or `SGPE_DB_URL`)
for anything real, and `database.auto_docker.enabled: false` to make a missing
URL an error instead. A container with that name that sgpe did not create is
left alone and reported.

How the modes use the config:

| Mode | Config file | Database |
|---|---|---|
| `local` | full layering as above | `database.url`, else the auto-started container |
| `docker` | `./sgpe.yaml` or `--config` mounted read-only | `SGPE_DB_URL`, else the same `sgpe-postgis` container `local` uses |
| `compose` | same mount | `SGPE_DB_URL`, else the bundled `postgres` service |

In the container modes the script runs the engine as your user, so a `0600`
`sgpe.yaml` stays readable. The listen port inside the container is fixed;
publish a different one with `--port`.

### Credentials

All upstream keys are optional; a missing key disables that source and the
startup log says so. Put them in `.env` or under `credentials:` in the YAML file.
See [.env.example](.env.example) for where to register.

| Variable | YAML key | Enables |
|---|---|---|
| `URA_ACCESS_KEY` | `ura_access_key` | `ura_txn`, `ura_rental`, `ura_pipeline`, `ura_developer_sales` |
| `ONEMAP_EMAIL` / `ONEMAP_PASSWORD` | `onemap_email` / `onemap_password` | OneMap Themes POIs (kindergartens, hawker centres, parks, hospitals). Geocoding search works without them. |
| `LTA_ACCOUNT_KEY` | `lta_account_key` | Bus stops |
| `GOOGLE_MAPS_API_KEY` | `google_maps_api_key` | Geocoding fallback when OneMap has no result |

No data is bundled with this repository and none is redistributed — the engine
pulls from the publishers under your own registration. Read
[docs/DATA_TERMS.md](docs/DATA_TERMS.md) before deploying.

## API

There is no authentication on any route, including `POST /sync/run` and the
ops page. Keep the port on a private network or behind a proxy that adds auth;
the compose file and launcher publish it on all interfaces by default.

Single listener, JSON in and out. List endpoints return
`{rows, total, limit, offset}`; stats endpoints return `{buckets: [{key, value, count}]}`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | DB ping |
| GET | `/sync/status` | latest run per source + known sources |
| POST | `/sync/run?source=&mode=incremental\|backfill[&arg=]` | trigger a job (202 with `run_id`; 409 if running) |
| GET | `/sync/runs[?source=]` | run history, newest first (paged) |
| GET | `/sync/runs/{id}` | one `sync_runs` row with stage/progress |
| GET | `/db/tables` | row count, on-disk size and last write per table |
| GET | `/db/tables/{table}/rows` | raw rows of one table, 100 per page (max 200); geography as WKT, bytea as hex |
| GET | `/ui/` | ops page (embedded static files) |
| POST | `/mcp` | MCP over streamable HTTP; see [MCP](#mcp) |
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

## MCP

Every `sgpe serve` also serves the query API as MCP tools at `/mcp`
(streamable HTTP). `sgpe mcp` serves the same tools over stdio for clients that
launch a command; it needs no database, only a running engine to call.

```bash
# Claude Code, over HTTP
claude mcp add --transport http sgpe http://localhost:8081/mcp

# Any stdio client: local binary, or inside the docker-mode container
sgpe mcp --url http://localhost:8081
docker exec -i sgpe-engine /sgpe mcp
```

For clients configured with JSON:

```json
{ "mcpServers": { "sgpe": { "command": "/path/to/sgpe", "args": ["mcp", "--url", "http://localhost:8081"] } } }
```

| Tool | What it answers |
|---|---|
| `search_properties` | turn a partial name into the exact project or HDB block |
| `get_project`, `find_projects` | project facts and nearest stations; projects by district, type, location |
| `hdb_resale_transactions`, `hdb_resale_stats` | HDB resale rows; median psf / price / count by month, town or flat type |
| `private_transactions`, `private_rentals`, `private_rental_trend` | URA sales and rentals; monthly rent percentiles |
| `development_pipeline`, `developer_sales` | upcoming projects; new-launch sales by month |
| `nearest_mrt`, `find_pois`, `poi_types` | stations and other POIs around a place |
| `hottest_projects`, `district_median` | 90-day volume ranking; district median price and psf |
| `cea_salesperson`, `cea_practice_stats` | CEA register lookup; a salesperson's last 12 months |
| `data_freshness` | when each source last synced, so "no rows" is not misread as "none exist" |

The tools are read-only; starting syncs stays with the API and the ops page.
They are written for a model to call:

- **Places are resolved by the server.** `near` takes a project name, an MRT
  station (`poi_type: mrt_exit`), an HDB block and street, or coordinates. The
  server turns it into coordinates and returns the resolved `anchor` with the
  results. A name it cannot find is an error, never a search of the whole island.
- **Inputs are normalised in code.** "Bishan", "4-room", "D9", "condo" and
  "2024-03" become the stored forms (`BISHAN`, `4 ROOM`, `09`, `Condominium`,
  `2024-03-01`). Out-of-range districts, unknown market segments and
  coordinates outside Singapore are rejected with a message that says why.
- **Pages are small.** 20 rows by default and at most 100, with `total` for paging.

`/mcp` refuses cross-origin browser requests and, on a loopback connection, any
`Host` other than localhost (DNS-rebinding protection). Like the rest of the
API it has no authentication; see the note at the top of [API](#api).

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
make test        # unit tests (race detector), no services needed
make smoke       # end-to-end over a throwaway PostGIS container
make vet
```

`make smoke` starts a disposable PostGIS container, sets `TEST_DATABASE_URL`,
runs the `-tags=smoke` suite, and removes the container after the tests finish.
You do not need to set `TEST_DATABASE_URL` yourself. To use an existing PostGIS
instance instead, set the variable and run the tests directly:

```bash
TEST_DATABASE_URL=postgres://postgres:test@localhost:55434/postgres?sslmode=disable \
  go test -tags=smoke -count=1 -p 1 ./...
```

Smoke tests clone a migrated template database per test, so they are isolated
and fast after the first run. Upstream clients are tested against
`httptest` servers through `sources/sourcestest`, which redirects a client's
requests without touching the network. Schema changes are migrations under
`migrations/` — never boot-time `ALTER`s.

## License

[MIT](LICENSE). Copyright 2026 Mr.Cyprinus (https://github.com/Cyprinus12138).

The licence covers this code only. Data fetched by the engine remains subject
to each publisher's terms — see [docs/DATA_TERMS.md](docs/DATA_TERMS.md).
