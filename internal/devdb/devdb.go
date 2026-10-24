// Package devdb gives a development run a PostGIS database without any
// setup: when no database URL is configured, sgpe asks devdb for one, and
// devdb starts (or reuses) a local Docker container and returns its URL.
//
// It shells out to the docker CLI rather than linking the Docker SDK — the
// CLI is what a developer already has, and it honours their contexts,
// DOCKER_HOST and credential helpers for free.
//
// The container is a convenience, not a deployment: it binds to 127.0.0.1
// only, uses a fixed local password, and keeps its data in a named volume
// so restarts do not lose a backfill. Containers devdb creates carry
// ManagedLabel; a container with the configured name but without the label
// is left alone and reported as a conflict.
package devdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ManagedLabel marks containers created by devdb.
const ManagedLabel = "io.github.cyprinus12138.sgpe.managed"

const (
	dbUser     = "postgres"
	dbPassword = "sgpe"
	dbName     = "sgpe"
	pgPort     = "5432/tcp"
)

// Options says which container to use and how to create it if missing.
type Options struct {
	Image     string
	Container string
	Volume    string
	// Port is the host port bound on 127.0.0.1; 0 lets Docker choose.
	Port int
	// ReadyTimeout bounds the wait for Postgres to accept connections.
	// Zero means 90 seconds — a first start runs initdb and creates the
	// PostGIS extension, which is slow on a cold machine.
	ReadyTimeout time.Duration
}

// Runner executes one docker CLI invocation and returns its combined
// output. The production runner is CLI; tests substitute a fake.
type Runner interface {
	Docker(ctx context.Context, args ...string) (string, error)
}

// CLI runs the docker binary found on PATH.
type CLI struct{}

func (CLI) Docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ErrNoDocker means the docker CLI is missing or its daemon unreachable.
var ErrNoDocker = errors.New("docker is not available")

// pollInterval is a var so tests do not wait.
var pollInterval = time.Second

// Ensure makes sure the container exists, runs and accepts connections,
// and returns a Postgres URL for it.
func Ensure(ctx context.Context, r Runner, o Options) (string, error) {
	if o.Container == "" || o.Image == "" {
		return "", errors.New("devdb: container and image are required")
	}
	if out, err := r.Docker(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDocker, firstLine(out, err))
	}

	state, err := inspect(ctx, r, o.Container)
	if err != nil {
		return "", err
	}
	switch {
	case !state.exists:
		slog.Info("devdb: creating local PostGIS container", "container", o.Container, "image", o.Image, "volume", o.Volume)
		if out, err := r.Docker(ctx, runArgs(o)...); err != nil {
			return "", fmt.Errorf("devdb: docker run: %s", firstLine(out, err))
		}
	case !state.managed:
		return "", fmt.Errorf("devdb: a container named %q exists but was not created by sgpe; "+
			"set database.auto_docker.container to another name or configure database.url", o.Container)
	case !state.running:
		slog.Info("devdb: starting stopped container", "container", o.Container)
		if out, err := r.Docker(ctx, "start", o.Container); err != nil {
			return "", fmt.Errorf("devdb: docker start: %s", firstLine(out, err))
		}
	default:
		slog.Info("devdb: reusing running container", "container", o.Container)
	}

	port, err := hostPort(ctx, r, o.Container)
	if err != nil {
		return "", err
	}
	if err := waitReady(ctx, r, o); err != nil {
		return "", err
	}
	return URL(port), nil
}

// URL is the connection string for a devdb container published on port.
func URL(port int) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", dbUser, dbPassword, port, dbName)
}

type containerState struct{ exists, running, managed bool }

func inspect(ctx context.Context, r Runner, name string) (containerState, error) {
	out, err := r.Docker(ctx, "container", "inspect", "--format",
		`{{.State.Running}} {{index .Config.Labels "`+ManagedLabel+`"}}`, name)
	if err != nil {
		if strings.Contains(strings.ToLower(out), "no such") {
			return containerState{}, nil
		}
		return containerState{}, fmt.Errorf("devdb: docker inspect: %s", firstLine(out, err))
	}
	fields := strings.Fields(out)
	return containerState{
		exists:  true,
		running: len(fields) > 0 && fields[0] == "true",
		managed: len(fields) > 1 && fields[1] == "true",
	}, nil
}

func runArgs(o Options) []string {
	publish := "127.0.0.1::5432"
	if o.Port > 0 {
		publish = fmt.Sprintf("127.0.0.1:%d:5432", o.Port)
	}
	args := []string{"run", "-d",
		"--name", o.Container,
		"--label", ManagedLabel + "=true",
		"-e", "POSTGRES_USER=" + dbUser,
		"-e", "POSTGRES_PASSWORD=" + dbPassword,
		"-e", "POSTGRES_DB=" + dbName,
		"-p", publish,
	}
	if o.Volume != "" {
		args = append(args, "-v", o.Volume+":/var/lib/postgresql/data")
	}
	return append(args, o.Image)
}

// hostPort reads the published port back from Docker, so a reused
// container created with a different (or random) port still resolves.
func hostPort(ctx context.Context, r Runner, name string) (int, error) {
	out, err := r.Docker(ctx, "port", name, pgPort)
	if err != nil {
		return 0, fmt.Errorf("devdb: docker port: %s", firstLine(out, err))
	}
	// One line per binding, e.g. "127.0.0.1:55432" or "[::]:55432".
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(line[i+1:]); err == nil && p > 0 {
			return p, nil
		}
	}
	return 0, fmt.Errorf("devdb: container %s does not publish %s (got %q)", name, pgPort, out)
}

// waitReady polls pg_isready over TCP inside the container. TCP matters:
// during first-boot initialisation the image runs a temporary server on
// the Unix socket only, which a socket check would wrongly accept.
func waitReady(ctx context.Context, r Runner, o Options) error {
	timeout := o.ReadyTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last string
	for {
		out, err := r.Docker(ctx, "exec", o.Container, "pg_isready", "-h", "127.0.0.1", "-U", dbUser, "-d", dbName)
		if err == nil {
			return nil
		}
		last = firstLine(out, err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("devdb: %s not ready after %s: %s", o.Container, timeout, last)
		case <-time.After(pollInterval):
		}
	}
}

func firstLine(out string, err error) string {
	if out = strings.TrimSpace(out); out != "" {
		line, _, _ := strings.Cut(out, "\n")
		return line
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
