// Command sgpe is the sg-property-engine binary.
//
//	sgpe serve                      migrate, start the scheduler and serve the query API
//	sgpe sync --source hdb --mode backfill [--arg X]
//	                                run one ingest job in the foreground and exit
//	sgpe migrate                    apply pending migrations and exit
//	sgpe config                     print the effective config (secrets masked)
//	sgpe mcp [--url URL]            MCP server on stdio, backed by a running engine
//
// Configuration is layered: built-in defaults, then a YAML file (--config,
// $SGPE_CONFIG, or ./sgpe.yaml — see sgpe.example.yaml), then environment
// variables (see .env.example; a .env file in the working directory is
// loaded too). With no database configured, a local PostGIS container is
// started through Docker.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.yaml.in/yaml/v3"

	"github.com/Cyprinus12138/sg-property-engine/client"
	"github.com/Cyprinus12138/sg-property-engine/db"
	"github.com/Cyprinus12138/sg-property-engine/httpapi"
	"github.com/Cyprinus12138/sg-property-engine/ingest"
	"github.com/Cyprinus12138/sg-property-engine/internal/devdb"
	"github.com/Cyprinus12138/sg-property-engine/mcpserver"
	"github.com/Cyprinus12138/sg-property-engine/migrations"
	"github.com/Cyprinus12138/sg-property-engine/scheduler"
	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	_ = godotenv.Load()

	configPath, args, err := extractConfigFlag(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "sgpe:", err)
		os.Exit(2)
	}
	if len(args) > 0 && (args[0] == "help" || isHelpFlag(args[0])) {
		usage()
		return
	}
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	cfg, err := LoadConfig(configPath, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sgpe:", err)
		os.Exit(2)
	}
	// stdout carries the protocol in stdio MCP mode; logs go to stderr there.
	logOut := os.Stdout
	if cmd == "mcp" {
		logOut = os.Stderr
	}
	setupLogging(logOut, cfg.Server.LogLevel)
	if cfg.Source != "" {
		slog.Info("config loaded", "file", cfg.Source)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var runErr error
	switch cmd {
	case "serve", "sync", "migrate":
		if runErr = resolveDatabase(ctx, &cfg, devdb.CLI{}); runErr != nil {
			break
		}
		switch cmd {
		case "serve":
			runErr = serve(ctx, cfg)
		case "sync":
			runErr = syncOnce(ctx, cfg, args)
		case "migrate":
			runErr = migrateOnly(ctx, cfg)
		}
	case "config":
		runErr = printConfig(os.Stdout, cfg)
	case "mcp":
		runErr = runMCP(ctx, cfg, args)
	default:
		usage()
		runErr = fmt.Errorf("unknown command %q", cmd)
	}
	if runErr != nil {
		slog.Error("sgpe exited with error", "cmd", cmd, "err", runErr)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: sgpe [--config FILE] [serve|sync|migrate|config|mcp]

  serve     (default) apply migrations, start the cron scheduler, serve the query API
  sync      run one ingest job in the foreground:
              sgpe sync --source hdb --mode backfill|incremental [--arg value]
  migrate   apply pending migrations and exit
  config    print the effective configuration with secrets masked
  mcp       serve MCP tools on stdin/stdout, querying a running engine:
              sgpe mcp [--url http://localhost:8081]   (default: this config's port)
            A running 'sgpe serve' also serves MCP over HTTP at /mcp.

  --config FILE   YAML config (default: $SGPE_CONFIG, else ./sgpe.yaml if present).
                  Environment variables override the file; see sgpe.example.yaml.`)
}

func isHelpFlag(s string) bool { return s == "-h" || s == "--help" || s == "-help" }

// extractConfigFlag pulls --config/-c (in either "--config x" or
// "--config=x" form) out of args, wherever it appears, so it works before
// or after the subcommand.
func extractConfigFlag(args []string) (string, []string, error) {
	var path string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config" || a == "-c":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a file path", a)
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="), strings.HasPrefix(a, "-config="):
			_, path, _ = strings.Cut(a, "=")
		default:
			rest = append(rest, a)
		}
	}
	return path, rest, nil
}

// resolveDatabase fills cfg.Database.URL when it is empty by starting the
// local PostGIS container, unless auto_docker is disabled.
func resolveDatabase(ctx context.Context, cfg *Config, r devdb.Runner) error {
	if cfg.Database.URL != "" {
		return nil
	}
	ad := cfg.Database.AutoDocker
	if !ad.Enabled {
		return errors.New("no database configured: set database.url in the config file or SGPE_DB_URL " +
			"(or enable database.auto_docker to start a local PostGIS container)")
	}
	slog.Info("no database configured; using a local PostGIS container", "container", ad.Container)
	url, err := devdb.Ensure(ctx, r, devdb.Options{
		Image: ad.Image, Container: ad.Container, Volume: ad.Volume, Port: ad.Port,
	})
	if err != nil {
		if errors.Is(err, devdb.ErrNoDocker) {
			return fmt.Errorf("no database configured and %w; install Docker or set database.url / SGPE_DB_URL", err)
		}
		return err
	}
	cfg.Database.URL = url
	slog.Info("local database ready", "url", redactURL(url))
	return nil
}

func printConfig(w io.Writer, cfg Config) error {
	if cfg.Source != "" {
		fmt.Fprintf(w, "# loaded from %s, then environment overrides\n", cfg.Source)
	} else {
		fmt.Fprintln(w, "# no config file; defaults plus environment overrides")
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(cfg.Redacted()); err != nil {
		return err
	}
	return enc.Close()
}

func setupLogging(w io.Writer, level string) {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level)) // validated in LoadConfig; zero value is info
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})))
}

// openStore connects and brings the schema up to date. Every subcommand
// goes through here so a fresh database works with any entry point.
func openStore(ctx context.Context, cfg Config) (*store.Store, error) {
	s, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	if err := db.RunMigrations(ctx, s.Pool, migrations.FS); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return s, nil
}

// engine bundles the upstream clients and ingestors so serve and sync
// build them identically.
type engine struct {
	ura  *sources.URA
	lta  *sources.LTA
	jobs []scheduler.Job
}

func buildEngine(cfg Config, s *store.Store) engine {
	dg := sources.NewDataGov()
	ura := sources.NewURA(cfg.Credentials.URAAccessKey)
	om := sources.NewOneMap(sources.OneMapCreds{Email: cfg.Credentials.OneMapEmail, Password: cfg.Credentials.OneMapPassword})
	gm := sources.NewGoogleMaps(sources.GoogleMapsCreds{APIKey: cfg.Credentials.GoogleMapsAPIKey})
	lta := sources.NewLTA(sources.LTACreds{AccountKey: cfg.Credentials.LTAAccountKey})

	resolver := &ingest.GeoResolver{OneMap: om, GoogleMaps: gm, Store: s}
	hdb := &ingest.HDBIngestor{DG: dg, Store: s}
	uraTxn := &ingest.URATxnIngestor{URA: ura, Store: s}
	uraRental := &ingest.URARentalIngestor{URA: ura, Store: s}
	uraPipe := &ingest.URAPipelineIngestor{URA: ura, Store: s}
	uraDevSales := &ingest.URADeveloperSalesIngestor{URA: ura, Store: s}
	geo := &ingest.GeoIngestor{Store: s, Resolver: resolver}
	pois := &ingest.POIIngestor{OneMap: om, LTA: lta, DG: dg, Store: s, Resolver: resolver, Themes: cfg.POIThemes}
	cea := &ingest.CEAIngestor{DG: dg, Store: s}
	ceaTxn := &ingest.CEATransactionsIngestor{DG: dg, Store: s}
	nearbyMRT := &ingest.NearbyMRTIngestor{Store: s}

	return engine{
		ura: ura,
		lta: lta,
		jobs: []scheduler.Job{
			{Source: "hdb", Cron: cfg.Cron.HDB, Run: hdb.Run},
			{Source: "ura_txn", Cron: cfg.Cron.URATxn, Run: uraTxn.Run},
			{Source: "ura_rental", Cron: cfg.Cron.URARental, Run: uraRental.Run},
			{Source: "ura_pipeline", Cron: cfg.Cron.URAPipeline, Run: uraPipe.Run},
			{Source: "ura_developer_sales", Cron: cfg.Cron.URADeveloperSales, Run: uraDevSales.Run},
			{Source: "geo", Cron: cfg.Cron.Geo, Run: geo.Run},
			{Source: "pois", Cron: cfg.Cron.POIs, Run: pois.Run},
			{Source: "cea", Cron: cfg.Cron.CEA, Run: cea.Run},
			{Source: "cea_txn", Cron: cfg.Cron.CEATxn, Run: ceaTxn.Run},
			{Source: "nearby_mrt", Cron: cfg.Cron.NearbyMRT, Run: nearbyMRT.Run},
		},
	}
}

func serve(ctx context.Context, cfg Config) error {
	s, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	eng := buildEngine(cfg, s)
	sched := scheduler.New(s)
	for _, job := range eng.jobs {
		if err := sched.Register(job); err != nil {
			return fmt.Errorf("register %s: %w", job.Source, err)
		}
		cron := job.Cron
		if cron == "" {
			cron = "off (manual trigger only)"
		}
		slog.Info("schedule", "source", job.Source, "cron", cron)
	}
	sched.Start()
	defer sched.Stop()

	if cfg.Server.BootstrapOnBoot {
		bootstrapMissing(ctx, s, sched)
	}

	srv := &httpapi.Server{Store: s, Sched: sched}
	// MCP tools call the same handlers in-process; see package mcpserver.
	srv.MCP = mcpserver.HTTPHandler(mcpserver.New(mcpserver.InProcessClient(srv.Routes()), version))
	httpSrv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Server.MetricsAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			if err := http.ListenAndServe(cfg.Server.MetricsAddr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics listener exited", "err", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("sgpe listening",
			"addr", cfg.Addr(),
			"sources", sched.Sources(),
			"ura_configured", eng.ura.HasKey(),
			"lta_configured", eng.lta.HasKey(),
			"bootstrap_on_boot", cfg.Server.BootstrapOnBoot)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http: %w", err)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// runMCP serves the MCP tools over stdio against an engine reached over
// HTTP. It needs no database of its own: point it at any running engine,
// e.g. the one `scripts/stack.sh up` started.
func runMCP(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	engineURL := fs.String("url", "", "engine base URL (default http://localhost:<server.port>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := *engineURL
	if u == "" {
		u = "http://localhost:" + cfg.Server.Port
	}
	c := client.New(u)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := c.SyncStatus(pingCtx); err != nil {
		// Keep serving: the engine may come up after the MCP client starts,
		// and each tool call reports its own connection error.
		slog.Warn("engine not reachable yet", "url", u, "err", err)
	}
	slog.Info("mcp on stdio", "engine", u)
	return mcpserver.RunStdio(ctx, mcpserver.New(c, version))
}

// syncOnce runs a single job in the foreground. Useful for cron-less
// deployments and for first-time backfills from a shell.
func syncOnce(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	source := fs.String("source", "", "source name (hdb, ura_txn, ura_rental, ura_pipeline, ura_developer_sales, geo, pois, cea, cea_txn, nearby_mrt)")
	mode := fs.String("mode", store.ModeIncremental, "incremental | backfill")
	arg := fs.String("arg", "", "optional per-run argument for parameterised sources")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		fs.Usage()
		return fmt.Errorf("--source is required")
	}
	if *mode != store.ModeIncremental && *mode != store.ModeBackfill {
		return fmt.Errorf("--mode must be %s or %s", store.ModeIncremental, store.ModeBackfill)
	}

	s, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	eng := buildEngine(cfg, s)
	sched := scheduler.New(s)
	for _, job := range eng.jobs {
		job.Cron = "" // foreground run: never arm the cron
		if err := sched.Register(job); err != nil {
			return fmt.Errorf("register %s: %w", job.Source, err)
		}
	}
	id, err := sched.TriggerArg(ctx, *source, *mode, *arg)
	if err != nil {
		return fmt.Errorf("trigger %s: %w", *source, err)
	}
	slog.Info("sync started", "source", *source, "mode", *mode, "run_id", id)
	sched.WaitDone(ctx, *source)
	run, err := s.GetRun(context.Background(), id)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run %d vanished", id)
	}
	slog.Info("sync finished", "source", run.Source, "status", run.Status,
		"rows_in", run.RowsIn, "rows_upserted", run.RowsUpserted, "error", run.Error)
	if run.Status != store.StatusSuccess {
		return fmt.Errorf("run %d ended with status %s: %s", id, run.Status, run.Error)
	}
	return nil
}

func migrateOnly(ctx context.Context, cfg Config) error {
	s, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	s.Close()
	slog.Info("migrations applied")
	return nil
}

// bootstrapMissing triggers a backfill for any source that has never
// recorded a successful run. Opt-in via server.bootstrap_on_boot — see the
// Config field for why it is off by default.
//
// Dependency order:
//   - hdb, ura_*, pois, cea, cea_txn — independent, fire immediately
//   - geo        — waits for hdb and ura_txn (geocodes blocks and projects)
//   - nearby_mrt — waits for pois and geo
func bootstrapMissing(ctx context.Context, s *store.Store, sched *scheduler.Scheduler) {
	done, err := s.SourcesWithSuccess(ctx)
	if err != nil {
		slog.Warn("bootstrap: query sync_runs failed", "err", err)
		return
	}
	trigger := func(src string) {
		if done[src] {
			return
		}
		id, err := sched.Trigger(ctx, src, store.ModeBackfill)
		if err != nil {
			slog.Warn("bootstrap trigger failed", "source", src, "err", err)
			return
		}
		slog.Info("bootstrap trigger", "source", src, "run_id", id)
	}
	for _, src := range []string{"hdb", "ura_txn", "ura_rental", "ura_pipeline", "ura_developer_sales", "pois", "cea", "cea_txn"} {
		trigger(src)
	}
	if !done["geo"] || !done["nearby_mrt"] {
		go func() {
			if !done["geo"] {
				sched.WaitDone(ctx, "ura_txn")
				sched.WaitDone(ctx, "hdb")
				trigger("geo")
			}
			if !done["nearby_mrt"] {
				sched.WaitDone(ctx, "pois")
				sched.WaitDone(ctx, "geo")
				trigger("nearby_mrt")
			}
		}()
	}
}
