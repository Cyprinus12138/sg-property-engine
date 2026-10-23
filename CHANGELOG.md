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
