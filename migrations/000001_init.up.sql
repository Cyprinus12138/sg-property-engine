-- 000001_init — sg-property-engine schema.
--
-- PostGIS is a hard dependency: every table with a location carries a
-- geography(Point, 4326) column so distance math is in metres without
-- manual projection, plus a GiST index so ST_DWithin filters are indexed.

CREATE EXTENSION IF NOT EXISTS postgis;

-- ─── Ingest bookkeeping ─────────────────────────────────────────────────────

-- One row per ingest run. The scheduler inserts status='running' on
-- trigger and finalises it on completion; 'skipped' means another process
-- held the per-source advisory lock.
CREATE TABLE sync_runs (
    id            bigserial PRIMARY KEY,
    source        text        NOT NULL,
    mode          text        NOT NULL,            -- incremental | backfill
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    status        text        NOT NULL,            -- running | success | error | skipped
    rows_in       integer     NOT NULL DEFAULT 0,
    rows_upserted integer     NOT NULL DEFAULT 0,
    error         text,
    stage         text,
    progress      numeric(5,2)
);
CREATE INDEX sync_runs_source_started_idx ON sync_runs (source, started_at DESC);

-- ─── HDB (data.gov.sg) ──────────────────────────────────────────────────────

-- HDB resale transactions. source_hash is md5 of the natural-key tuple so
-- a refetch is an idempotent no-op.
CREATE TABLE hdb_resale (
    id              bigserial PRIMARY KEY,
    month           date          NOT NULL,        -- first day of month
    town            text          NOT NULL,
    block           text          NOT NULL,
    street_name     text          NOT NULL,
    flat_type       text          NOT NULL,
    storey_range    text,
    floor_area_sqm  numeric(7,2),
    flat_model      text,
    lease_commence  integer,
    remaining_lease text,
    resale_price    numeric(12,2) NOT NULL,
    source_hash     bytea         NOT NULL UNIQUE,
    ingested_at     timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX hdb_resale_town_flat_month_idx ON hdb_resale (town, flat_type, month DESC);
CREATE INDEX hdb_resale_street_block_idx    ON hdb_resale (street_name, block);
CREATE INDEX hdb_resale_month_idx           ON hdb_resale (month);

-- One row per physical HDB block. Block facts come from the HDB Property
-- Information dataset; coordinates are filled lazily by the geo ingest.
CREATE TABLE hdb_blocks (
    street_name          text        NOT NULL,
    block                text        NOT NULL,
    name                 text,                      -- estate/building name from OneMap
    postal               text,
    lat                  double precision,
    lng                  double precision,
    geom                 geography(Point, 4326),
    planning_area        text,
    year_completed       integer,
    max_floor_lvl        integer,
    total_dwelling_units integer,
    residential          boolean,
    town_code            text,
    geocoded_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (street_name, block)
);
CREATE INDEX hdb_blocks_geom_idx ON hdb_blocks USING gist (geom);

-- ─── URA Data Service ───────────────────────────────────────────────────────

CREATE TABLE ura_transactions (
    id             bigserial PRIMARY KEY,
    project        text          NOT NULL,
    street         text,
    district       text,
    property_type  text,
    tenure         text,
    market_segment text,
    area_sqm       numeric(8,2),
    price          numeric(14,2) NOT NULL,
    psf            numeric(10,2),
    contract_date  date          NOT NULL,        -- month precision; day = 1
    floor_range    text,
    type_of_sale   text,
    source_hash    bytea         NOT NULL UNIQUE,
    ingested_at    timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX ura_txn_district_date_idx ON ura_transactions (district, contract_date DESC);
CREATE INDEX ura_txn_project_date_idx  ON ura_transactions (project, contract_date DESC);

-- URA publishes rental floor area as a band for privacy; both edges kept.
CREATE TABLE ura_rentals (
    id            bigserial PRIMARY KEY,
    project       text          NOT NULL,
    street        text,
    district      text,
    property_type text,
    area_sqm_min  numeric(8,2),
    area_sqm_max  numeric(8,2),
    rent          numeric(10,2) NOT NULL,
    lease_date    date          NOT NULL,
    bedroom_num   numeric(3,2),
    source_hash   bytea         NOT NULL UNIQUE,
    ingested_at   timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX ura_rent_district_date_idx ON ura_rentals (district, lease_date DESC);
CREATE INDEX ura_rent_project_date_idx  ON ura_rentals (project, lease_date DESC);

-- Small snapshot, fully refreshed (TRUNCATE + INSERT) on every run.
CREATE TABLE ura_pipeline (
    id             bigserial PRIMARY KEY,
    project        text        NOT NULL,
    developer      text,
    street         text,
    district       text,
    market_segment text,
    total_units    integer,
    units_sold     integer,
    expected_top   text,
    status         text,
    raw            jsonb,
    ingested_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ura_pipeline_district_idx ON ura_pipeline (district);

-- Monthly developer-sales snapshots keyed on (project, ref_period).
CREATE TABLE ura_developer_sales (
    id                bigserial PRIMARY KEY,
    project           text          NOT NULL,
    developer         text,
    street            text,
    district          text,
    market_segment    text,
    property_type     text,
    ref_period        date          NOT NULL,     -- first day of month
    units_avail       integer,
    launched_to_date  integer,
    sold_to_date      integer,
    launched_in_month integer,
    sold_in_month     integer,
    lowest_price      numeric(10,2),
    median_price      numeric(10,2),
    highest_price     numeric(10,2),
    ingested_at       timestamptz   NOT NULL DEFAULT now(),
    UNIQUE (project, ref_period)
);
CREATE INDEX ura_devsales_district_period_idx ON ura_developer_sales (district, ref_period DESC);
CREATE INDEX ura_devsales_period_idx          ON ura_developer_sales (ref_period DESC);
CREATE INDEX ura_devsales_project_period_idx  ON ura_developer_sales (project, ref_period DESC);

-- ─── Derived master tables ──────────────────────────────────────────────────

-- Project master, derived from ura_transactions (plus pipeline-only new
-- launches). Geo columns and nearby_mrt are owned by the enrichment jobs
-- and survive rebuilds.
CREATE TABLE projects (
    name                 text PRIMARY KEY,         -- URA project name, UPPERCASE
    street               text,
    district             text,
    property_types       text[],
    tenure               text,
    market_segment       text,
    postal               text,
    lat                  double precision,
    lng                  double precision,
    geom                 geography(Point, 4326),
    planning_area        text,
    first_txn_date       date,
    last_txn_date        date,
    txn_count            integer,
    nearby_mrt           jsonb       NOT NULL DEFAULT '[]'::jsonb,
    nearby_mrt_synced_at timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX projects_district_idx ON projects (district);
CREATE INDEX projects_geom_idx     ON projects USING gist (geom);

-- Per-building records discovered from OneMap while geocoding a project.
CREATE TABLE project_buildings (
    id            bigserial PRIMARY KEY,
    project       text        NOT NULL,
    blk_no        text        NOT NULL,
    road_name     text        NOT NULL,
    building_name text,
    postal        text,
    lat           double precision,
    lng           double precision,
    geom          geography(Point, 4326),
    discovered_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project, postal)
);
CREATE INDEX project_buildings_project_idx ON project_buildings (project);
CREATE INDEX project_buildings_geom_idx    ON project_buildings USING gist (geom);

-- ─── Geo ────────────────────────────────────────────────────────────────────

-- Type-tagged points of interest (MRT/LRT exits, bus stops, schools, OneMap
-- themes). One row per feature; source identifies the upstream feed so a
-- per-source sync can prune rows that disappeared upstream.
CREATE TABLE pois (
    id          bigserial PRIMARY KEY,
    type        text        NOT NULL,
    name        text        NOT NULL,
    subtype     text,
    lat         double precision NOT NULL,
    lng         double precision NOT NULL,
    geom        geography(Point, 4326) NOT NULL,
    attrs       jsonb,
    source      text        NOT NULL,
    source_hash bytea       NOT NULL UNIQUE,
    ingested_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pois_geom_idx      ON pois USING gist (geom);
CREATE INDEX pois_type_idx      ON pois (type);
CREATE INDEX pois_type_name_idx ON pois (type, name);

-- Geocoder memo (OneMap / Google Places), including negative results.
CREATE TABLE geo_cache (
    query         text PRIMARY KEY,
    postal        text,
    lat           double precision,
    lng           double precision,
    planning_area text,
    raw           jsonb,
    fetched_at    timestamptz NOT NULL DEFAULT now()
);

-- ─── CEA (data.gov.sg) ──────────────────────────────────────────────────────

-- Current-state snapshot of the CEA salesperson register.
CREATE TABLE cea_salespersons (
    registration_no         text PRIMARY KEY,
    salesperson_name        text        NOT NULL,
    registration_start_date date,
    registration_end_date   date,
    estate_agent_name       text,
    estate_agent_license_no text,
    ingested_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cea_salespersons_license_idx ON cea_salespersons (estate_agent_license_no);

-- Residential transaction records attributed to salespersons. Month
-- precision only; source_hash covers every field because the feed has
-- no transaction id.
CREATE TABLE cea_transactions (
    id                  bigserial PRIMARY KEY,
    salesperson_reg_num text        NOT NULL,
    salesperson_name    text        NOT NULL,
    transaction_month   date        NOT NULL,
    property_type       text,
    transaction_type    text,
    represented         text,
    town                text,
    district            text,
    general_location    text,
    source_hash         bytea       NOT NULL UNIQUE,
    ingested_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cea_txn_reg_month_idx           ON cea_transactions (salesperson_reg_num, transaction_month DESC);
CREATE INDEX cea_txn_month_idx               ON cea_transactions (transaction_month DESC);
CREATE INDEX cea_txn_proptype_kind_month_idx ON cea_transactions (property_type, transaction_type, transaction_month DESC);
CREATE INDEX cea_txn_town_idx                ON cea_transactions (town);
