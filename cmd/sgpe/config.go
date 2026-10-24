package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is everything the engine reads at startup. Values are layered,
// later layers winning:
//
//  1. built-in defaults (defaultConfig)
//  2. a YAML file — --config, else $SGPE_CONFIG, else ./sgpe.yaml if present
//  3. environment variables (envBindings), including a .env file
//
// sgpe.example.yaml documents every key; .env.example documents the
// variables.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Database    DatabaseConfig    `yaml:"database"`
	Credentials CredentialsConfig `yaml:"credentials"`
	Cron        CronConfig        `yaml:"cron"`

	// POIThemes are the OneMap themes ingested as POIs (friendly names or
	// raw OneMap queryNames).
	POIThemes []string `yaml:"poi_themes"`

	// Source is the config file that was loaded, empty when none was.
	Source string `yaml:"-"`
}

type ServerConfig struct {
	// Port is a bare port ("8081") or a listen address (":8081",
	// "127.0.0.1:8081").
	Port     string `yaml:"port"`
	LogLevel string `yaml:"log_level"`

	// MetricsAddr is the Prometheus scrape listener. Empty disables it.
	MetricsAddr string `yaml:"metrics_addr"`

	// BootstrapOnBoot triggers a backfill at startup for every source that
	// has never recorded a successful run. Off by default: a fresh database
	// stays empty until an operator triggers the first backfill, so boot is
	// always fast and a wiped sync_runs table never fires every backfill at
	// once.
	BootstrapOnBoot bool `yaml:"bootstrap_on_boot"`
}

type DatabaseConfig struct {
	// URL is a Postgres connection string; the server needs the PostGIS
	// extension available. Empty means "use AutoDocker".
	URL string `yaml:"url"`

	AutoDocker AutoDockerConfig `yaml:"auto_docker"`
}

// AutoDockerConfig controls the local PostGIS container started when no
// database URL is configured. It is a development convenience: the
// container binds to 127.0.0.1 only and uses a fixed local password.
type AutoDockerConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Image     string `yaml:"image"`
	Container string `yaml:"container"`
	Volume    string `yaml:"volume"`
	// Port is the host port on 127.0.0.1; 0 lets Docker pick a free one.
	Port int `yaml:"port"`
}

type CredentialsConfig struct {
	URAAccessKey     string `yaml:"ura_access_key"`
	OneMapEmail      string `yaml:"onemap_email"`
	OneMapPassword   string `yaml:"onemap_password"`
	LTAAccountKey    string `yaml:"lta_account_key"`
	GoogleMapsAPIKey string `yaml:"google_maps_api_key"`
}

// CronConfig holds one cron spec per source, in server-local time. An
// empty spec disables the schedule for that source (manual triggers still
// work).
type CronConfig struct {
	HDB               string `yaml:"hdb"`
	CEA               string `yaml:"cea"`
	CEATxn            string `yaml:"cea_txn"`
	URATxn            string `yaml:"ura_txn"`
	URARental         string `yaml:"ura_rental"`
	URAPipeline       string `yaml:"ura_pipeline"`
	URADeveloperSales string `yaml:"ura_developer_sales"`
	Geo               string `yaml:"geo"`
	POIs              string `yaml:"pois"`
	NearbyMRT         string `yaml:"nearby_mrt"`
}

// defaultConfigFile is picked up from the working directory when neither
// --config nor $SGPE_CONFIG names a file.
const defaultConfigFile = "sgpe.yaml"

// defaultConfig returns the built-in defaults. Cron slots are
// Singapore-friendly off-peak times, ordered so dependent jobs (geo,
// nearby_mrt) run after the feeds they read.
func defaultConfig() Config {
	return Config{
		Server: ServerConfig{Port: "8081", LogLevel: "info"},
		Database: DatabaseConfig{AutoDocker: AutoDockerConfig{
			Enabled:   true,
			Image:     "postgis/postgis:16-3.4",
			Container: "sgpe-postgis",
			Volume:    "sgpe-pgdata",
			Port:      55432,
		}},
		Cron: CronConfig{
			HDB:               "0 2 * * *",
			CEA:               "30 2 * * *",
			CEATxn:            "45 2 * * *",
			URATxn:            "0 3 * * *",
			URARental:         "30 3 * * *",
			URAPipeline:       "45 3 * * *",
			URADeveloperSales: "0 4 * * *",
			Geo:               "15 4 * * *",
			POIs:              "0 5 * * 1",
			NearbyMRT:         "0 6 * * *",
		},
		POIThemes: []string{"kindergartens", "hawker", "park", "hospital"},
	}
}

// envBinding maps one environment variable onto a Config field. set
// receives the trimmed, non-empty value.
type envBinding struct {
	name string
	set  func(c *Config, v string) error
}

func str(f func(c *Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *f(c) = v; return nil }
}

func boolean(f func(c *Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("not a boolean: %q", v)
		}
		*f(c) = b
		return nil
	}
}

func integer(f func(c *Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("not an integer: %q", v)
		}
		*f(c) = n
		return nil
	}
}

// envBindings is the full list of environment overrides, in the order they
// apply. DATABASE_URL comes before SGPE_DB_URL so the specific name wins
// when both are set.
var envBindings = []envBinding{
	{"SGPE_PORT", str(func(c *Config) *string { return &c.Server.Port })},
	{"SGPE_LOG_LEVEL", str(func(c *Config) *string { return &c.Server.LogLevel })},
	{"SGPE_METRICS_ADDR", str(func(c *Config) *string { return &c.Server.MetricsAddr })},
	{"SGPE_BOOTSTRAP_ON_BOOT", boolean(func(c *Config) *bool { return &c.Server.BootstrapOnBoot })},

	{"DATABASE_URL", str(func(c *Config) *string { return &c.Database.URL })},
	{"SGPE_DB_URL", str(func(c *Config) *string { return &c.Database.URL })},
	{"SGPE_DB_AUTO_DOCKER", boolean(func(c *Config) *bool { return &c.Database.AutoDocker.Enabled })},
	{"SGPE_DB_DOCKER_IMAGE", str(func(c *Config) *string { return &c.Database.AutoDocker.Image })},
	{"SGPE_DB_DOCKER_CONTAINER", str(func(c *Config) *string { return &c.Database.AutoDocker.Container })},
	{"SGPE_DB_DOCKER_VOLUME", str(func(c *Config) *string { return &c.Database.AutoDocker.Volume })},
	{"SGPE_DB_DOCKER_PORT", integer(func(c *Config) *int { return &c.Database.AutoDocker.Port })},

	{"URA_ACCESS_KEY", str(func(c *Config) *string { return &c.Credentials.URAAccessKey })},
	{"ONEMAP_EMAIL", str(func(c *Config) *string { return &c.Credentials.OneMapEmail })},
	{"ONEMAP_PASSWORD", str(func(c *Config) *string { return &c.Credentials.OneMapPassword })},
	{"LTA_ACCOUNT_KEY", str(func(c *Config) *string { return &c.Credentials.LTAAccountKey })},
	{"GOOGLE_MAPS_API_KEY", str(func(c *Config) *string { return &c.Credentials.GoogleMapsAPIKey })},

	{"SGPE_CRON_HDB", str(func(c *Config) *string { return &c.Cron.HDB })},
	{"SGPE_CRON_CEA", str(func(c *Config) *string { return &c.Cron.CEA })},
	{"SGPE_CRON_CEA_TXN", str(func(c *Config) *string { return &c.Cron.CEATxn })},
	{"SGPE_CRON_URA_TXN", str(func(c *Config) *string { return &c.Cron.URATxn })},
	{"SGPE_CRON_URA_RENT", str(func(c *Config) *string { return &c.Cron.URARental })},
	{"SGPE_CRON_URA_PIPE", str(func(c *Config) *string { return &c.Cron.URAPipeline })},
	{"SGPE_CRON_URA_DEV_SALES", str(func(c *Config) *string { return &c.Cron.URADeveloperSales })},
	{"SGPE_CRON_GEO", str(func(c *Config) *string { return &c.Cron.Geo })},
	{"SGPE_CRON_POIS", str(func(c *Config) *string { return &c.Cron.POIs })},
	{"SGPE_CRON_NEARBY_MRT", str(func(c *Config) *string { return &c.Cron.NearbyMRT })},

	{"SGPE_POI_THEMES", func(c *Config, v string) error { c.POIThemes = splitList(v); return nil }},
}

// LoadConfig builds the effective config. path is the --config flag value;
// when empty, $SGPE_CONFIG and then ./sgpe.yaml are tried. A file named
// explicitly (flag or env) must exist; the implicit ./sgpe.yaml is optional.
func LoadConfig(path string, getenv func(string) string) (Config, error) {
	c := defaultConfig()

	explicit := true
	if path == "" {
		path = strings.TrimSpace(getenv("SGPE_CONFIG"))
	}
	if path == "" {
		path, explicit = defaultConfigFile, false
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decodeYAML(raw, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		c.Source = path
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// No implicit config file: defaults + environment only.
	default:
		return c, fmt.Errorf("config: %w", err)
	}

	for _, b := range envBindings {
		v := strings.TrimSpace(getenv(b.name))
		if v == "" {
			continue
		}
		if err := b.set(&c, v); err != nil {
			return c, fmt.Errorf("%s: %w", b.name, err)
		}
	}
	return c, c.validate()
}

// decodeYAML decodes strictly: an unknown key is an error, so a typo such
// as `ura_acess_key` fails loudly instead of silently disabling a source.
// An empty file is valid and leaves the defaults untouched.
func decodeYAML(raw []byte, c *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c *Config) validate() error {
	c.Server.LogLevel = strings.ToLower(strings.TrimSpace(c.Server.LogLevel))
	switch c.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("server.log_level must be debug, info, warn or error, got %q", c.Server.LogLevel)
	}
	if strings.TrimSpace(c.Server.Port) == "" {
		return fmt.Errorf("server.port is empty")
	}
	if p := c.Database.AutoDocker.Port; p < 0 || p > 65535 {
		return fmt.Errorf("database.auto_docker.port out of range: %d", p)
	}
	var themes []string
	for _, t := range c.POIThemes {
		if t = strings.TrimSpace(t); t != "" {
			themes = append(themes, t)
		}
	}
	c.POIThemes = themes
	return nil
}

// Addr is the HTTP listen address derived from Server.Port.
func (c Config) Addr() string {
	p := strings.TrimSpace(c.Server.Port)
	if strings.Contains(p, ":") {
		return p
	}
	return ":" + p
}

// Redacted returns a copy safe to print: secrets and the database
// password are masked.
func (c Config) Redacted() Config {
	mask := func(s string) string {
		if s == "" {
			return ""
		}
		return "***"
	}
	c.Credentials.URAAccessKey = mask(c.Credentials.URAAccessKey)
	c.Credentials.OneMapPassword = mask(c.Credentials.OneMapPassword)
	c.Credentials.LTAAccountKey = mask(c.Credentials.LTAAccountKey)
	c.Credentials.GoogleMapsAPIKey = mask(c.Credentials.GoogleMapsAPIKey)
	c.Database.URL = redactURL(c.Database.URL)
	c.POIThemes = append([]string(nil), c.POIThemes...)
	return c
}

// redactURL masks the password in a postgres:// URL, leaving everything
// else readable. A key=value DSN (no scheme) is masked whole.
func redactURL(s string) string {
	if s == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return "***"
	}
	// The userinfo ends at the last '@' of the authority, which itself ends
	// at the first '/' or '?' — so an '@' in the query is not mistaken for
	// it, while an unescaped '@' in the password still is handled.
	authority := rest
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority = rest[:i]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return s
	}
	userinfo, host := rest[:at], rest[at+1:]
	if user, _, hasPass := strings.Cut(userinfo, ":"); hasPass {
		return scheme + "://" + user + ":***@" + host
	}
	return s
}

func splitList(v string) []string {
	var out []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
