#!/usr/bin/env bash
# One-command launcher for sg-property-engine.
#
#   scripts/stack.sh up     [compose|docker|local] [options]   start and wait until healthy
#   scripts/stack.sh down   [--purge]                          stop whatever this script started
#   scripts/stack.sh status                                    what is running, and /sync/status
#   scripts/stack.sh logs   [compose|docker|local]             follow the engine's logs
#
# Modes (default: compose):
#   compose  docker compose: PostGIS + engine image, the bundled DB always
#   docker   plain `docker run`, no compose needed: engine image + the same
#            sgpe-postgis container `local` mode uses, on a private network
#   local    build and run the Go binary on the host; with no database
#            configured it starts sgpe-postgis itself (needs Go >= 1.25)
#
# Options for `up`:
#   --config FILE     YAML config (default: ./sgpe.yaml if present)
#   --port N          host port for the API (default 8081)
#   --backfill LIST   once healthy, trigger backfills, e.g. --backfill hdb,ura_txn
#   --no-build        reuse the existing image / binary
#   --foreground      local mode only: stay attached (Ctrl-C stops the engine)
#
# `down --purge` also deletes the database volumes. Credentials go in .env
# or the YAML file — see .env.example and sgpe.example.yaml.
#
# Portable bash 3.2 (macOS default): no associative arrays, no ${x,,}.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE="$ROOT/.sgpe"
cd "$ROOT"

# setting KEY DEFAULT: the shell environment, else the last KEY= line of
# .env (surrounding quotes stripped), else DEFAULT. Empty counts as unset,
# matching the binary.
setting() {
  local v="${!1:-}"
  if [ -z "$v" ] && [ -f "$ROOT/.env" ]; then
    v="$(grep -E "^[[:space:]]*(export[[:space:]]+)?$1=" "$ROOT/.env" | tail -n 1 | cut -d= -f2- || true)"
    v="${v%\"}"; v="${v#\"}"; v="${v%\'}"; v="${v#\'}"
  fi
  printf '%s' "${v:-$2}"
}

# Names shared with the binary's auto_docker defaults (cmd/sgpe/config.go)
# so local and docker modes use one database. The scripts read these from
# the environment or .env; a rename made only in sgpe.yaml reaches the
# binary (local mode) but not docker mode.
DB_CONTAINER="$(setting SGPE_DB_DOCKER_CONTAINER sgpe-postgis)"
DB_VOLUME="$(setting SGPE_DB_DOCKER_VOLUME sgpe-pgdata)"
DB_IMAGE="$(setting SGPE_DB_DOCKER_IMAGE postgis/postgis:16-3.4)"
DB_PORT="$(setting SGPE_DB_DOCKER_PORT 55432)"
MANAGED_LABEL="io.github.cyprinus12138.sgpe.managed"
ENGINE_CONTAINER="sgpe-engine"
NETWORK="sgpe"
IMAGE="sgpe:local"

if [ -t 2 ]; then C_B=$'\033[1;34m' C_Y=$'\033[1;33m' C_R=$'\033[1;31m' C_0=$'\033[0m'; else C_B="" C_Y="" C_R="" C_0=""; fi
log()  { printf '%s==>%s %s\n' "$C_B" "$C_0" "$*" >&2; }
warn() { printf '%swarn:%s %s\n' "$C_Y" "$C_0" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$C_R" "$C_0" "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required${2:+ — $2}"; }

need_docker() {
  need docker "install Docker Desktop or Docker Engine"
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable (is it running?)"
}

http_get() {
  if command -v curl >/dev/null 2>&1; then curl -fsS --max-time 5 "$1"
  elif command -v wget >/dev/null 2>&1; then wget -qO- --timeout=5 "$1"
  else die "curl or wget is required"; fi
}

http_post() {
  if command -v curl >/dev/null 2>&1; then curl -fsS --max-time 10 -X POST "$1"
  else wget -qO- --timeout=10 --post-data='' "$1"; fi
}

wait_healthy() {
  local url="http://localhost:$1/healthz" deadline=$((SECONDS + ${2:-180}))
  log "waiting for $url"
  until http_get "$url" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      die "engine not healthy after ${2:-180}s — check: scripts/stack.sh logs"
    fi
    if [ -n "${3:-}" ] && ! kill -0 "$3" 2>/dev/null; then
      die "engine exited during startup — see $STATE/sgpe.log"
    fi
    sleep 2
  done
}

trigger_backfills() {
  local port="$1" list="$2" src
  [ -n "$list" ] || return 0
  local IFS=','
  for src in $list; do
    src="$(echo "$src" | tr -d '[:space:]')"
    [ -n "$src" ] || continue
    log "backfill $src: $(http_post "http://localhost:$port/sync/run?source=$src&mode=backfill" || echo 'trigger failed')"
  done
  log "progress: curl http://localhost:$port/sync/status"
}

container_exists()  { docker container inspect "$1" >/dev/null 2>&1; }
container_running() { [ "$(docker container inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = "true" ]; }

create_db() {
  local publish="127.0.0.1:$DB_PORT:5432"
  [ "$DB_PORT" = "0" ] && publish="127.0.0.1::5432"
  docker run -d --name "$DB_CONTAINER" --label "$MANAGED_LABEL=true" \
    -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=sgpe -e POSTGRES_DB=sgpe \
    -p "$publish" -v "$1:/var/lib/postgresql/data" "$DB_IMAGE" >/dev/null
}

# ensure_db mirrors internal/devdb: create/start the labelled container and
# wait for TCP readiness. It refuses a same-named container it didn't make.
ensure_db() {
  if container_exists "$DB_CONTAINER"; then
    local managed
    managed="$(docker container inspect -f "{{index .Config.Labels \"$MANAGED_LABEL\"}}" "$DB_CONTAINER")"
    [ "$managed" = "true" ] || die "container $DB_CONTAINER exists but was not created by sgpe; set SGPE_DB_DOCKER_CONTAINER to another name"
    if ! container_running "$DB_CONTAINER"; then
      log "starting $DB_CONTAINER"
      if ! docker start "$DB_CONTAINER" >/dev/null 2>&1; then
        # A stopped container can hold a reference to a network that no
        # longer exists. The data lives in the volume, so recreate the
        # container on the volume it actually had.
        local vol
        vol="$(docker container inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql/data"}}{{.Name}}{{end}}{{end}}' "$DB_CONTAINER")"
        log "recreating $DB_CONTAINER (data kept in volume ${vol:-$DB_VOLUME})"
        docker rm -f "$DB_CONTAINER" >/dev/null
        create_db "${vol:-$DB_VOLUME}"
      fi
    fi
  else
    log "creating $DB_CONTAINER ($DB_IMAGE, volume $DB_VOLUME)"
    create_db "$DB_VOLUME"
  fi
  local deadline=$((SECONDS + 90))
  until docker exec "$DB_CONTAINER" pg_isready -h 127.0.0.1 -U postgres -d sgpe >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die "$DB_CONTAINER not ready after 90s: docker logs $DB_CONTAINER"
    sleep 1
  done
}

# resolve_config sets CONFIG_FILE to an absolute path, or empty.
resolve_config() {
  CONFIG_FILE="${1:-}"
  if [ -z "$CONFIG_FILE" ] && [ -f "$ROOT/sgpe.yaml" ]; then CONFIG_FILE="$ROOT/sgpe.yaml"; fi
  if [ -n "$CONFIG_FILE" ]; then
    [ -f "$CONFIG_FILE" ] || die "config file not found: $CONFIG_FILE"
    CONFIG_FILE="$(cd "$(dirname "$CONFIG_FILE")" && pwd)/$(basename "$CONFIG_FILE")"
    log "config: $CONFIG_FILE"
  fi
}

up_compose() {
  need_docker
  docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
  local args=(up -d)
  [ "$BUILD" = 1 ] && args+=(--build)
  SGPE_CONFIG_FILE="${CONFIG_FILE:-/dev/null}" SGPE_HOST_PORT="$PORT" SGPE_RUN_AS="$(id -u):$(id -g)" \
    docker compose "${args[@]}"
  wait_healthy "$PORT"
}

up_docker() {
  need_docker
  [ "$BUILD" = 1 ] && { log "building $IMAGE"; docker build -q -t "$IMAGE" . >/dev/null; }
  docker image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE not found; drop --no-build"
  docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK" >/dev/null
  local db_url; db_url="$(setting SGPE_DB_URL "")"
  if [ -z "$db_url" ]; then
    ensure_db
    docker network connect "$NETWORK" "$DB_CONTAINER" 2>/dev/null || true
    db_url="postgres://postgres:sgpe@$DB_CONTAINER:5432/sgpe?sslmode=disable"
  else
    log "using SGPE_DB_URL; no local database container"
  fi
  if container_exists "$ENGINE_CONTAINER"; then docker rm -f "$ENGINE_CONTAINER" >/dev/null; fi

  # --env-file first: docker gives the explicit -e flags precedence.
  local args=(run -d --name "$ENGINE_CONTAINER" --network "$NETWORK" -p "$PORT:8081"
    --user "$(id -u):$(id -g)" --restart unless-stopped)
  [ -f "$ROOT/.env" ] && args+=(--env-file "$ROOT/.env")
  args+=(-e SGPE_PORT=8081 -e SGPE_DB_AUTO_DOCKER=false -e "SGPE_DB_URL=$db_url")
  if [ -n "$CONFIG_FILE" ]; then
    args+=(-v "$CONFIG_FILE:/etc/sgpe/sgpe.yaml:ro" -e SGPE_CONFIG=/etc/sgpe/sgpe.yaml)
  fi
  docker "${args[@]}" "$IMAGE" serve >/dev/null
  wait_healthy "$PORT"
}

up_local() {
  need go "install Go 1.25+ from https://go.dev/dl/"
  mkdir -p "$STATE" "$ROOT/bin"
  if [ "$BUILD" = 1 ] || [ ! -x "$ROOT/bin/sgpe" ]; then
    log "building bin/sgpe"
    go build -o "$ROOT/bin/sgpe" ./cmd/sgpe
  fi
  local args=(serve)
  [ -n "$CONFIG_FILE" ] && args=(--config "$CONFIG_FILE" serve)
  if [ "$FOREGROUND" = 1 ]; then
    log "starting in the foreground (Ctrl-C to stop)"
    SGPE_PORT="$PORT" exec "$ROOT/bin/sgpe" "${args[@]}"
  fi
  if [ -f "$STATE/sgpe.pid" ] && kill -0 "$(cat "$STATE/sgpe.pid")" 2>/dev/null; then
    die "already running (pid $(cat "$STATE/sgpe.pid")); scripts/stack.sh down first"
  fi
  SGPE_PORT="$PORT" nohup "$ROOT/bin/sgpe" "${args[@]}" >"$STATE/sgpe.log" 2>&1 &
  echo $! >"$STATE/sgpe.pid"
  log "started pid $(cat "$STATE/sgpe.pid"), logs in .sgpe/sgpe.log"
  # The first run may be pulling the PostGIS image, hence the long budget.
  wait_healthy "$PORT" 600 "$(cat "$STATE/sgpe.pid")"
}

cmd_up() {
  MODE=compose; CONFIG_ARG=""; PORT=8081; BACKFILL=""; BUILD=1; FOREGROUND=0
  while [ $# -gt 0 ]; do
    case "$1" in
      compose|docker|local) MODE="$1" ;;
      --config)     CONFIG_ARG="${2:?--config needs a file}"; shift ;;
      --config=*)   CONFIG_ARG="${1#*=}" ;;
      --port)       PORT="${2:?--port needs a number}"; shift ;;
      --port=*)     PORT="${1#*=}" ;;
      --backfill)   BACKFILL="${2:?--backfill needs a source list}"; shift ;;
      --backfill=*) BACKFILL="${1#*=}" ;;
      --no-build)   BUILD=0 ;;
      --foreground|-f) FOREGROUND=1 ;;
      *) die "unknown option: $1 (see scripts/stack.sh help)" ;;
    esac
    shift
  done
  case "$PORT" in ''|*[!0-9]*) die "--port must be a number" ;; esac
  if [ "$FOREGROUND" = 1 ] && [ "$MODE" != local ]; then die "--foreground is for local mode"; fi
  if [ "$FOREGROUND" = 1 ] && [ -n "$BACKFILL" ]; then die "--backfill cannot follow --foreground; trigger it from another shell"; fi
  resolve_config "$CONFIG_ARG"
  mkdir -p "$STATE"; echo "$MODE" >"$STATE/mode"; echo "$PORT" >"$STATE/port"

  "up_$MODE"
  log "sg-property-engine is up ($MODE) on http://localhost:$PORT"
  trigger_backfills "$PORT" "$BACKFILL"
  if [ -z "$BACKFILL" ]; then
    log "the database starts empty; first data: scripts/stack.sh up $MODE --no-build --backfill hdb"
    log "  or: curl -X POST 'http://localhost:$PORT/sync/run?source=hdb&mode=backfill'"
  fi
}

cmd_down() {
  local purge=0
  case "${1:-}" in --purge) purge=1 ;; '') ;; *) die "unknown option: $1" ;; esac

  if [ -f "$STATE/sgpe.pid" ]; then
    local pid; pid="$(cat "$STATE/sgpe.pid")"
    if kill -0 "$pid" 2>/dev/null; then
      log "stopping local engine (pid $pid)"
      kill "$pid"
      for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    fi
    rm -f "$STATE/sgpe.pid"
  fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if [ -n "$(docker compose ps -aq 2>/dev/null)" ] || { [ "$purge" = 1 ] && docker volume inspect sgpe_postgres-data >/dev/null 2>&1; }; then
      log "docker compose down"
      if [ "$purge" = 1 ]; then docker compose down -v; else docker compose down; fi
    fi
    if container_exists "$ENGINE_CONTAINER"; then log "removing $ENGINE_CONTAINER"; docker rm -f "$ENGINE_CONTAINER" >/dev/null; fi
    # Database containers are found by label, so one renamed in sgpe.yaml
    # is still cleaned up.
    local id name vols
    for id in $(docker ps -aq --filter "label=$MANAGED_LABEL=true"); do
      name="$(docker container inspect -f '{{.Name}}' "$id")"; name="${name#/}"
      if [ "$purge" = 1 ]; then
        vols="$(docker container inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$id")"
        log "removing $name"; docker rm -f "$id" >/dev/null
        for v in $vols; do log "deleting volume $v"; docker volume rm "$v" >/dev/null; done
      elif container_running "$id"; then
        log "stopping $name (data kept in its volume; --purge deletes it)"; docker stop "$id" >/dev/null
      fi
      # Detach before the network goes, or the stopped container keeps a
      # reference to a deleted network and cannot start again.
      [ "$purge" = 1 ] || docker network disconnect -f "$NETWORK" "$id" >/dev/null 2>&1 || true
    done
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
  fi
  rm -f "$STATE/mode" "$STATE/port"
  log "down"
}

cmd_status() {
  local port="8081"; [ -f "$STATE/port" ] && port="$(cat "$STATE/port")"
  [ -f "$STATE/mode" ] && log "last started mode: $(cat "$STATE/mode")"
  if [ -f "$STATE/sgpe.pid" ] && kill -0 "$(cat "$STATE/sgpe.pid")" 2>/dev/null; then
    log "local engine: pid $(cat "$STATE/sgpe.pid")"
  fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    local ids; ids="$( { docker ps -aq --filter "label=com.docker.compose.project=sgpe"
                         docker ps -aq --filter "label=$MANAGED_LABEL=true"
                         docker ps -aq --filter "name=^/$ENGINE_CONTAINER\$"; } | sort -u)"
    if [ -n "$ids" ]; then
      # shellcheck disable=SC2086
      docker container inspect -f '{{.Name}}  {{.State.Status}}' $ids | sed 's#^/#  #'
    else
      log "no sgpe containers"
    fi
  fi
  if http_get "http://localhost:$port/healthz" >/dev/null 2>&1; then
    log "healthy on :$port — /sync/status:"
    http_get "http://localhost:$port/sync/status"; echo
  else
    warn "nothing answering on http://localhost:$port/healthz"
  fi
}

cmd_logs() {
  local mode="${1:-}"
  [ -z "$mode" ] && [ -f "$STATE/mode" ] && mode="$(cat "$STATE/mode")"
  case "${mode:-compose}" in
    compose) docker compose logs -f engine ;;
    docker)  docker logs -f "$ENGINE_CONTAINER" ;;
    local)   tail -f "$STATE/sgpe.log" ;;
    *) die "unknown mode: $mode" ;;
  esac
}

case "${1:-help}" in
  up)     shift; cmd_up "$@" ;;
  down)   shift; cmd_down "$@" ;;
  status) shift; cmd_status ;;
  logs)   shift; cmd_logs "$@" ;;
  help|-h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//' ;;
  *) die "unknown command: $1 (up | down | status | logs | help)" ;;
esac
