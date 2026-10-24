# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- Initial public release of the engine: HDB resale + property information
  (data.gov.sg), URA transactions / rentals / pipeline / developer sales,
  OneMap geocoding and Themes POIs, LTA DataMall bus stops, CEA salesperson
  register and transaction records — all ingested into Postgres + PostGIS
  with idempotent `source_hash` upserts.
- Cron scheduler with per-source Postgres advisory locks, `sync_runs`
  bookkeeping, throttled progress reporting and a 409 on concurrent triggers.
- Geo-aware query API: near-anchor (`near_lat/lng`, `near_project`,
  `near_poi_*`, `near_block/street`) and has-POI-within filters on every list
  endpoint; `/geo/nearest-stations`; `/explore/*` aggregations; unified
  `/search/properties` autocomplete; CEA lookups.
- `sgpe` binary with `serve`, `sync` (foreground single job) and `migrate`
  subcommands; `client` package for Go consumers.
- Minimal from-scratch migrations, docker-compose stack, CI (unit + PostGIS
  smoke), design doc and data-terms summary.
- YAML config file (`--config`, `$SGPE_CONFIG` or `./sgpe.yaml`) layered under
  environment variables, decoded strictly; `sgpe config` prints the effective
  config with secrets masked.
- With no database configured, `serve` / `sync` / `migrate` start or reuse a
  local PostGIS container through the Docker CLI.
- `scripts/stack.sh` one-command launcher (`up` / `down` / `status` / `logs`) for
  compose, plain docker and local-binary modes, plus a Makefile and a CI job
  that runs each mode.
- MCP server: 18 read-only tools over the query API, at `/mcp` on `sgpe serve`
  (streamable HTTP) and as `sgpe mcp` (stdio, against a running engine).
  Place anchors resolve to coordinates server-side and fail loudly when
  unknown; filter values are normalised to the stored forms.
- `client.POITypes` for `GET /pois/types`.
- Ops page at `/ui/`, embedded in the binary: per-source jobs with progress
  and run buttons, run history, and table stats with row preview. Backed by
  new endpoints `GET /sync/runs`, `GET /db/tables` and
  `GET /db/tables/{table}/rows`.
- Unit tests for every upstream client against `httptest` (`sources/sourcestest`),
  for config loading and for the local-database bootstrap.
