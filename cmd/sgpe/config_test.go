package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Cyprinus12138/sg-property-engine/internal/devdb"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// inTempDir runs the test from an empty directory so a developer's own
// ./sgpe.yaml never leaks into the result.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig_DefaultsWithoutFileOrEnv(t *testing.T) {
	inTempDir(t)
	c, err := LoadConfig("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
	if c.Addr() != ":8081" || c.Database.URL != "" || !c.Database.AutoDocker.Enabled {
		t.Errorf("addr=%s url=%q auto=%v", c.Addr(), c.Database.URL, c.Database.AutoDocker.Enabled)
	}
}

func TestLoadConfig_FileOverridesDefaults(t *testing.T) {
	dir := inTempDir(t)
	path := writeFile(t, filepath.Join(dir, "custom.yaml"), `
server:
  port: 9090
  log_level: DEBUG
database:
  url: postgres://u:p@db:5432/sgpe
  auto_docker:
    port: 0
credentials:
  ura_access_key: from-file
cron:
  pois: ""
poi_themes: [hawker, " park ", ""]
`)
	c, err := LoadConfig(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != path {
		t.Errorf("source = %q", c.Source)
	}
	if c.Addr() != ":9090" || c.Server.LogLevel != "debug" {
		t.Errorf("server = %+v", c.Server)
	}
	if c.Database.URL != "postgres://u:p@db:5432/sgpe" || c.Database.AutoDocker.Port != 0 {
		t.Errorf("database = %+v", c.Database)
	}
	// Keys the file omits keep their defaults.
	if c.Database.AutoDocker.Image != "postgis/postgis:16-3.4" || c.Cron.HDB != "0 2 * * *" {
		t.Errorf("defaults lost: image=%q cron.hdb=%q", c.Database.AutoDocker.Image, c.Cron.HDB)
	}
	if c.Cron.POIs != "" {
		t.Errorf("an explicit empty cron must disable the schedule, got %q", c.Cron.POIs)
	}
	if !reflect.DeepEqual(c.POIThemes, []string{"hawker", "park"}) {
		t.Errorf("themes = %q", c.POIThemes)
	}
}

func TestLoadConfig_EnvOverridesFile(t *testing.T) {
	dir := inTempDir(t)
	writeFile(t, filepath.Join(dir, "sgpe.yaml"), `
server: {port: "9090", bootstrap_on_boot: true}
database: {url: postgres://file/db}
credentials: {ura_access_key: from-file, lta_account_key: lta-file}
`)
	c, err := LoadConfig("", env(map[string]string{
		"SGPE_PORT":              "127.0.0.1:7000",
		"SGPE_BOOTSTRAP_ON_BOOT": "false",
		"DATABASE_URL":           "postgres://alias/db",
		"SGPE_DB_URL":            "postgres://env/db",
		"URA_ACCESS_KEY":         "from-env",
		"SGPE_POI_THEMES":        "hawker, hospital",
		"SGPE_DB_DOCKER_PORT":    "60001",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != "sgpe.yaml" {
		t.Errorf("implicit ./sgpe.yaml not picked up: source=%q", c.Source)
	}
	if c.Addr() != "127.0.0.1:7000" || c.Server.BootstrapOnBoot {
		t.Errorf("server = %+v", c.Server)
	}
	if c.Database.URL != "postgres://env/db" {
		t.Errorf("SGPE_DB_URL must win over DATABASE_URL and the file, got %q", c.Database.URL)
	}
	if c.Credentials.URAAccessKey != "from-env" || c.Credentials.LTAAccountKey != "lta-file" {
		t.Errorf("credentials = %+v", c.Credentials)
	}
	if !reflect.DeepEqual(c.POIThemes, []string{"hawker", "hospital"}) || c.Database.AutoDocker.Port != 60001 {
		t.Errorf("themes=%q port=%d", c.POIThemes, c.Database.AutoDocker.Port)
	}
}

func TestLoadConfig_DatabaseURLAlias(t *testing.T) {
	inTempDir(t)
	c, err := LoadConfig("", env(map[string]string{"DATABASE_URL": "postgres://alias/db"}))
	if err != nil || c.Database.URL != "postgres://alias/db" {
		t.Fatalf("url=%q err=%v", c.Database.URL, err)
	}
}

func TestLoadConfig_SGPEConfigEnvNamesTheFile(t *testing.T) {
	dir := inTempDir(t)
	path := writeFile(t, filepath.Join(dir, "elsewhere.yaml"), "server: {port: \"1234\"}\n")
	c, err := LoadConfig("", env(map[string]string{"SGPE_CONFIG": path}))
	if err != nil || c.Addr() != ":1234" {
		t.Fatalf("addr=%s err=%v", c.Addr(), err)
	}
	// The flag beats the variable.
	other := writeFile(t, filepath.Join(dir, "flag.yaml"), "server: {port: \"4321\"}\n")
	c, err = LoadConfig(other, env(map[string]string{"SGPE_CONFIG": path}))
	if err != nil || c.Addr() != ":4321" {
		t.Fatalf("addr=%s err=%v", c.Addr(), err)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	dir := inTempDir(t)
	cases := map[string]struct {
		file string
		env  map[string]string
		want string
	}{
		"explicit file missing":  {env: map[string]string{"SGPE_CONFIG": filepath.Join(dir, "nope.yaml")}, want: "no such file"},
		"unknown key":            {file: "credentials:\n  ura_acess_key: x\n", want: "ura_acess_key"},
		"wrong type":             {file: "server: {bootstrap_on_boot: maybe}\n", want: "maybe"},
		"bad log level":          {file: "server: {log_level: loud}\n", want: "log_level"},
		"bad bool env":           {env: map[string]string{"SGPE_DB_AUTO_DOCKER": "nah"}, want: "SGPE_DB_AUTO_DOCKER"},
		"bad int env":            {env: map[string]string{"SGPE_DB_DOCKER_PORT": "five"}, want: "SGPE_DB_DOCKER_PORT"},
		"port out of range":      {file: "database: {auto_docker: {port: 70000}}\n", want: "out of range"},
		"empty server port":      {file: "server: {port: \"\"}\n", want: "server.port"},
		"not a mapping at all":   {file: "- just\n- a list\n", want: "cannot unmarshal"},
		"two documents, one bad": {file: "server: {port: \"1\"}\n---\nbogus: 1\n", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = writeFile(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".yaml"), tc.file)
			}
			_, err := LoadConfig(path, env(tc.env))
			if tc.want == "" {
				// Only the first YAML document is read; later ones are ignored.
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfig_EmptyFileIsDefaults(t *testing.T) {
	dir := inTempDir(t)
	path := writeFile(t, filepath.Join(dir, "empty.yaml"), "")
	c, err := LoadConfig(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	c.Source = ""
	if !reflect.DeepEqual(c, defaultConfig()) {
		t.Errorf("empty file changed the defaults: %+v", c)
	}
}

// The shipped example must parse strictly and say exactly what the
// defaults are, or the documentation has drifted from the code.
func TestExampleConfigMatchesDefaults(t *testing.T) {
	c, err := LoadConfig("../../sgpe.example.yaml", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	c.Source = ""
	if !reflect.DeepEqual(c, defaultConfig()) {
		t.Errorf("sgpe.example.yaml differs from defaultConfig():\n got %+v\nwant %+v", c, defaultConfig())
	}
}

// Every environment binding must be documented in .env.example and named
// next to its key in sgpe.example.yaml.
func TestEnvBindingsAreDocumented(t *testing.T) {
	dotenv, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../sgpe.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range envBindings {
		if !bytes.Contains(dotenv, []byte(b.name)) {
			t.Errorf(".env.example does not mention %s", b.name)
		}
		if !bytes.Contains(example, []byte(b.name)) {
			t.Errorf("sgpe.example.yaml does not mention %s", b.name)
		}
	}
}

func TestRedacted(t *testing.T) {
	c := defaultConfig()
	c.Database.URL = "postgres://admin:hunter2@db.example:5432/sgpe?sslmode=require"
	c.Credentials = CredentialsConfig{URAAccessKey: "k", OneMapEmail: "me@example.com", OneMapPassword: "pw", LTAAccountKey: "l", GoogleMapsAPIKey: "g"}
	r := c.Redacted()
	if r.Database.URL != "postgres://admin:***@db.example:5432/sgpe?sslmode=require" {
		t.Errorf("url = %s", r.Database.URL)
	}
	if r.Credentials.URAAccessKey != "***" || r.Credentials.OneMapPassword != "***" ||
		r.Credentials.LTAAccountKey != "***" || r.Credentials.GoogleMapsAPIKey != "***" {
		t.Errorf("credentials = %+v", r.Credentials)
	}
	if r.Credentials.OneMapEmail != "me@example.com" {
		t.Error("the email is not a secret and should stay readable")
	}
	if c.Credentials.URAAccessKey != "k" {
		t.Error("Redacted must not mutate the receiver")
	}
	for in, want := range map[string]string{
		"":                                 "",
		"postgres://localhost/sgpe":        "postgres://localhost/sgpe",
		"postgres://user@localhost/sgpe":   "postgres://user@localhost/sgpe",
		"postgres://u:p@ss@w0rd@h/db":      "postgres://u:***@h/db",
		"host=localhost password=secret":   "***",
		"postgresql://u:pw@h1,h2/db?x=y@z": "postgresql://u:***@h1,h2/db?x=y@z",
		"postgres://h/db?opt=a@b":          "postgres://h/db?opt=a@b",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintConfigMasksSecrets(t *testing.T) {
	c := defaultConfig()
	c.Database.URL = "postgres://u:topsecret@h/db"
	c.Credentials.URAAccessKey = "ura-secret"
	var buf bytes.Buffer
	if err := printConfig(&buf, c); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "topsecret") || strings.Contains(out, "ura-secret") {
		t.Fatalf("secret leaked:\n%s", out)
	}
	// What printConfig emits must load back as a config file.
	dir := inTempDir(t)
	path := writeFile(t, filepath.Join(dir, "printed.yaml"), out)
	if _, err := LoadConfig(path, env(nil)); err != nil {
		t.Fatalf("printed config does not round-trip: %v", err)
	}
}

func TestExtractConfigFlag(t *testing.T) {
	cases := []struct {
		in       []string
		path     string
		rest     []string
		wantFail bool
	}{
		{in: nil, rest: []string{}},
		{in: []string{"--config", "a.yaml", "serve"}, path: "a.yaml", rest: []string{"serve"}},
		{in: []string{"sync", "--source", "hdb", "-c", "b.yaml"}, path: "b.yaml", rest: []string{"sync", "--source", "hdb"}},
		{in: []string{"--config=c.yaml", "migrate"}, path: "c.yaml", rest: []string{"migrate"}},
		{in: []string{"serve", "--config"}, wantFail: true},
	}
	for _, c := range cases {
		path, rest, err := extractConfigFlag(c.in)
		if c.wantFail {
			if err == nil {
				t.Errorf("%v: want error", c.in)
			}
			continue
		}
		if err != nil || path != c.path || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%v: path=%q rest=%q err=%v", c.in, path, rest, err)
		}
	}
}

type fakeRunner struct {
	err   error
	calls int
}

func (f *fakeRunner) Docker(_ context.Context, args ...string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	switch args[0] {
	case "container":
		return "true true", nil
	case "port":
		return "127.0.0.1:55432", nil
	}
	return "", nil
}

func TestResolveDatabase(t *testing.T) {
	ctx := context.Background()

	t.Run("configured URL is left alone", func(t *testing.T) {
		c := defaultConfig()
		c.Database.URL = "postgres://x/y"
		r := &fakeRunner{}
		if err := resolveDatabase(ctx, &c, r); err != nil || c.Database.URL != "postgres://x/y" || r.calls != 0 {
			t.Fatalf("url=%q calls=%d err=%v", c.Database.URL, r.calls, err)
		}
	})
	t.Run("auto docker fills the URL", func(t *testing.T) {
		c := defaultConfig()
		if err := resolveDatabase(ctx, &c, &fakeRunner{}); err != nil {
			t.Fatal(err)
		}
		if c.Database.URL != devdb.URL(55432) {
			t.Errorf("url = %q", c.Database.URL)
		}
	})
	t.Run("auto docker disabled", func(t *testing.T) {
		c := defaultConfig()
		c.Database.AutoDocker.Enabled = false
		r := &fakeRunner{}
		err := resolveDatabase(ctx, &c, r)
		if err == nil || !strings.Contains(err.Error(), "SGPE_DB_URL") || r.calls != 0 {
			t.Fatalf("calls=%d err=%v", r.calls, err)
		}
	})
	t.Run("docker missing", func(t *testing.T) {
		c := defaultConfig()
		err := resolveDatabase(ctx, &c, &fakeRunner{err: errors.New(`exec: "docker": executable file not found in $PATH`)})
		if !errors.Is(err, devdb.ErrNoDocker) || !strings.Contains(err.Error(), "install Docker") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAddr(t *testing.T) {
	for in, want := range map[string]string{"8081": ":8081", ":9000": ":9000", "127.0.0.1:8081": "127.0.0.1:8081", " 80 ": ":80"} {
		c := Config{Server: ServerConfig{Port: in}}
		if got := c.Addr(); got != want {
			t.Errorf("Addr(%q) = %q, want %q", in, got, want)
		}
	}
}
