package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is everything the engine reads from the environment. See
// .env.example for the documented list and where to obtain each key.
type Config struct {
	DBURL    string
	Addr     string
	LogLevel string

	// MetricsAddr is the Prometheus scrape listener. Empty disables it.
	MetricsAddr string

	URAAccessKey     string
	OneMapEmail      string
	OneMapPassword   string
	GoogleMapsAPIKey string
	LTAAccountKey    string

	CronHDB        string
	CronURATxn     string
	CronURARent    string
	CronURAPipe    string
	CronURADevSale string
	CronGeo        string
	CronPOIs       string
	CronCEA        string
	CronCEATxn     string
	CronNearbyMRT  string

	POIThemes []string

	// BootstrapOnBoot triggers a backfill at startup for every source that
	// has never recorded a successful run. Off by default: a fresh database
	// stays empty until an operator triggers the first backfill, so boot is
	// always fast and a wiped sync_runs table never fires every backfill at
	// once.
	BootstrapOnBoot bool
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// LoadConfig reads the environment. Cron defaults are Singapore-friendly
// off-peak slots, ordered so dependent jobs (geo, nearby_mrt) run after
// the feeds they read.
func LoadConfig() (Config, error) {
	c := Config{
		DBURL:            envOr("SGPE_DB_URL", os.Getenv("DATABASE_URL")),
		Addr:             ":" + envOr("SGPE_PORT", "8081"),
		LogLevel:         envOr("SGPE_LOG_LEVEL", "info"),
		MetricsAddr:      os.Getenv("SGPE_METRICS_ADDR"),
		URAAccessKey:     os.Getenv("URA_ACCESS_KEY"),
		OneMapEmail:      os.Getenv("ONEMAP_EMAIL"),
		OneMapPassword:   os.Getenv("ONEMAP_PASSWORD"),
		GoogleMapsAPIKey: os.Getenv("GOOGLE_MAPS_API_KEY"),
		LTAAccountKey:    os.Getenv("LTA_ACCOUNT_KEY"),

		CronHDB:        envOr("SGPE_CRON_HDB", "0 2 * * *"),
		CronCEA:        envOr("SGPE_CRON_CEA", "30 2 * * *"),
		CronCEATxn:     envOr("SGPE_CRON_CEA_TXN", "45 2 * * *"),
		CronURATxn:     envOr("SGPE_CRON_URA_TXN", "0 3 * * *"),
		CronURARent:    envOr("SGPE_CRON_URA_RENT", "30 3 * * *"),
		CronURAPipe:    envOr("SGPE_CRON_URA_PIPE", "45 3 * * *"),
		CronURADevSale: envOr("SGPE_CRON_URA_DEV_SALES", "0 4 * * *"),
		CronGeo:        envOr("SGPE_CRON_GEO", "15 4 * * *"),
		CronPOIs:       envOr("SGPE_CRON_POIS", "0 5 * * 1"),
		CronNearbyMRT:  envOr("SGPE_CRON_NEARBY_MRT", "0 6 * * *"),

		BootstrapOnBoot: envBool("SGPE_BOOTSTRAP_ON_BOOT"),
	}
	if strings.HasPrefix(envOr("SGPE_PORT", "8081"), ":") {
		c.Addr = envOr("SGPE_PORT", "8081")
	}
	if c.DBURL == "" {
		return c, fmt.Errorf("SGPE_DB_URL (or DATABASE_URL) is required")
	}
	for _, t := range strings.Split(envOr("SGPE_POI_THEMES", "kindergartens,hawker,park,hospital"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.POIThemes = append(c.POIThemes, t)
		}
	}
	return c, nil
}
