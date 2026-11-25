# svc-catalog. Private modules: GOPRIVATE=github.com/hostryhq/* (and credentials for it). Local development uses an
# uncommitted go.work listing ../go-common, ../go-auth and ../api-specs-p2 (the Phase 2 contracts).
SHELL := /bin/bash
export GOPRIVATE ?= github.com/hostryhq/*
COMPOSE ?= docker compose -p catalog -f ../infra/dev/compose.yaml
export HOSTRY_PG_PORT ?= 55440
export CATALOG_TEST_PG_ADMIN_DSN ?= postgres://hostry:hostry-dev-only@127.0.0.1:$(HOSTRY_PG_PORT)/hostry?sslmode=disable

.PHONY: gen vet lint test race integration contract dev-up dev-down tidy build

# Regenerate internal/gen from ../api-specs-p2 (override API_SPECS_DIR); records the spec commit in
# internal/gen/SPEC_VERSION.
gen:
	./scripts/gen.sh

build:
	go build ./...

vet:
	go vet ./... && go vet -tags integration ./...

# golangci-lint when installed, plus the outlet-scope lint of ADR-0003 A1.4 (also run by `go test`).
lint: vet
	gofmt -l . | (! grep .)
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; CI runs it"
	go run github.com/hostryhq/go-auth/cmd/scopelint -tables scopelint.tables.json ./repository/pg ./service

# Unit tests, the contract suite against the double and the HTTP suites on the double (no database needed).
test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

# Integration tests: real Postgres, real service, shared contract suite, isolation and outlet-scope suites.
integration:
	go test -tags integration -race -count=1 ./...

contract:
	go test -count=1 ./double/ -run Contract
	go test -tags integration -count=1 ./api/ -run RealServiceContract

# Postgres from the shared dev stack, under its own compose project and host port.
dev-up:
	$(COMPOSE) up -d --wait postgres

dev-down:
	$(COMPOSE) down -v

tidy:
	GOWORK=off go mod tidy
