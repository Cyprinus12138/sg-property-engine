# Thin wrappers; scripts/stack.sh is the source of truth (run it with no
# arguments for help). MODE is compose | docker | local.
MODE ?= compose
ARGS ?=

.PHONY: up down status logs dev build test smoke vet fmt

up:            ## start the stack and wait until healthy
	scripts/stack.sh up $(MODE) $(ARGS)

down:          ## stop it (make down ARGS=--purge also deletes the data)
	scripts/stack.sh down $(ARGS)

status:
	scripts/stack.sh status

logs:
	scripts/stack.sh logs $(MODE)

dev:           ## run the binary in the foreground; starts a local PostGIS if no DB is configured
	scripts/stack.sh up local --foreground $(ARGS)

build:
	go build -o bin/sgpe ./cmd/sgpe

test:          ## unit tests, no services needed
	go test -race -count=1 ./...

# Throwaway PostGIS on 55434, torn down afterwards.
smoke:
	@docker rm -f sgpe-smoke >/dev/null 2>&1 || true
	docker run -d --rm --name sgpe-smoke -e POSTGRES_PASSWORD=test -p 127.0.0.1:55434:5432 postgis/postgis:16-3.4 >/dev/null
	@until docker exec sgpe-smoke pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done
	TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:55434/postgres?sslmode=disable' \
	  go test -tags=smoke -count=1 -p 1 ./... ; status=$$?; docker stop sgpe-smoke >/dev/null; exit $$status

vet:
	go vet ./... && go vet -tags=smoke ./...

fmt:
	gofmt -w .
