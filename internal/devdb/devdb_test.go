package devdb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeDocker answers docker CLI calls from a table keyed by the first
// argument, and records every call.
type fakeDocker struct {
	calls   [][]string
	answers map[string]func(args []string) (string, error)
}

func (f *fakeDocker) Docker(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if h, ok := f.answers[args[0]]; ok {
		return h(args)
	}
	return "", nil
}

func (f *fakeDocker) called(verb string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func ok(out string) func([]string) (string, error) {
	return func([]string) (string, error) { return out, nil }
}

func fail(out string) func([]string) (string, error) {
	return func([]string) (string, error) { return out, errors.New("exit status 1") }
}

var opts = Options{Image: "postgis/postgis:16-3.4", Container: "sgpe-postgis", Volume: "sgpe-pgdata", Port: 55432}

func init() { pollInterval = time.Millisecond }

func TestEnsure_CreatesMissingContainer(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"version":   ok("27.3.1"),
		"container": fail("Error: No such container: sgpe-postgis"),
		"port":      ok("127.0.0.1:55432"),
	}}
	url, err := Ensure(context.Background(), f, opts)
	if err != nil {
		t.Fatal(err)
	}
	if url != "postgres://postgres:sgpe@127.0.0.1:55432/sgpe?sslmode=disable" {
		t.Errorf("url = %s", url)
	}
	runs := f.called("run")
	if len(runs) != 1 {
		t.Fatalf("run calls = %v", f.calls)
	}
	args := strings.Join(runs[0], " ")
	for _, want := range []string{
		"--name sgpe-postgis",
		"--label " + ManagedLabel + "=true",
		"-p 127.0.0.1:55432:5432",
		"-v sgpe-pgdata:/var/lib/postgresql/data",
		"-e POSTGRES_DB=sgpe",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("docker run args missing %q: %s", want, args)
		}
	}
	if runs[0][len(runs[0])-1] != opts.Image {
		t.Errorf("image must be the last arg: %v", runs[0])
	}
	if len(f.called("exec")) != 1 {
		t.Errorf("readiness must be checked once it answers: %v", f.calls)
	}
}

func TestEnsure_RandomPortWhenZero(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": fail("Error response from daemon: No such container: x"),
		"port":      ok("127.0.0.1:49153\n[::1]:49153"),
	}}
	o := opts
	o.Port, o.Volume = 0, ""
	url, err := Ensure(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.called("run")[0], " ")
	if !strings.Contains(args, "-p 127.0.0.1::5432") || strings.Contains(args, " -v ") {
		t.Errorf("args = %s", args)
	}
	if !strings.Contains(url, ":49153/") {
		t.Errorf("url = %s, want the port docker assigned", url)
	}
}

func TestEnsure_StartsStoppedManagedContainer(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("false true"),
		"port":      ok("127.0.0.1:60000"),
	}}
	url, err := Ensure(context.Background(), f, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.called("start")) != 1 || len(f.called("run")) != 0 {
		t.Errorf("calls = %v", f.calls)
	}
	// The port Docker reports wins over the configured one: the container
	// may have been created with a different setting.
	if !strings.Contains(url, ":60000/") {
		t.Errorf("url = %s", url)
	}
}

func TestEnsure_ReusesRunningContainer(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("true true"),
		"port":      ok("127.0.0.1:55432"),
	}}
	if _, err := Ensure(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
	if len(f.called("start"))+len(f.called("run")) != 0 {
		t.Errorf("a running container must not be started or created: %v", f.calls)
	}
}

func TestEnsure_RefusesForeignContainer(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("true <no value>"),
	}}
	_, err := Ensure(context.Background(), f, opts)
	if err == nil || !strings.Contains(err.Error(), "not created by sgpe") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("must not touch a foreign container: %v", f.calls)
	}
}

func TestEnsure_NoDocker(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"version": fail("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"),
	}}
	_, err := Ensure(context.Background(), f, opts)
	if !errors.Is(err, ErrNoDocker) || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsure_WaitsUntilReady(t *testing.T) {
	n := 0
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("true true"),
		"port":      ok("127.0.0.1:55432"),
		"exec": func([]string) (string, error) {
			if n++; n < 3 {
				return "127.0.0.1:5432 - no response", errors.New("exit status 2")
			}
			return "127.0.0.1:5432 - accepting connections", nil
		},
	}}
	if _, err := Ensure(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("pg_isready calls = %d, want 3", n)
	}
	exec := strings.Join(f.called("exec")[0], " ")
	if !strings.Contains(exec, "pg_isready -h 127.0.0.1") {
		t.Errorf("readiness must probe TCP, got %s", exec)
	}
}

func TestEnsure_ReadyTimeout(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("true true"),
		"port":      ok("127.0.0.1:55432"),
		"exec":      fail("127.0.0.1:5432 - no response"),
	}}
	o := opts
	o.ReadyTimeout = 20 * time.Millisecond
	_, err := Ensure(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "no response") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsure_UnpublishedPort(t *testing.T) {
	f := &fakeDocker{answers: map[string]func([]string) (string, error){
		"container": ok("true true"),
		"port":      ok(""),
	}}
	if _, err := Ensure(context.Background(), f, opts); err == nil || !strings.Contains(err.Error(), "does not publish") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsure_RequiresNameAndImage(t *testing.T) {
	if _, err := Ensure(context.Background(), &fakeDocker{}, Options{}); err == nil {
		t.Fatal("empty options must fail")
	}
}
