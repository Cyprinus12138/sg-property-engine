-- 000001_init — reversal. Development reset only; this drops every table
-- the engine owns. The PostGIS extension is left in place because other
-- schemas in the same database may depend on it.

DROP TABLE IF EXISTS cea_transactions;
DROP TABLE IF EXISTS cea_salespersons;
DROP TABLE IF EXISTS geo_cache;
DROP TABLE IF EXISTS pois;
DROP TABLE IF EXISTS project_buildings;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS ura_developer_sales;
DROP TABLE IF EXISTS ura_pipeline;
DROP TABLE IF EXISTS ura_rentals;
DROP TABLE IF EXISTS ura_transactions;
DROP TABLE IF EXISTS hdb_blocks;
DROP TABLE IF EXISTS hdb_resale;
DROP TABLE IF EXISTS sync_runs;
