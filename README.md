# svc-catalog

Catalog service: menus, items, variants, modifiers, item tax classes, stations, floors, tables, printers, reason codes and the
CSV menu import (ADR-0014). Devices write these masters as sync ops; catalog projects them from the bus into its own database,
serves them over a read API, and writes cloud-side changes (tax class, menu import) back as ops through the gateway emit API.
Contract: `api-specs/openapi/catalog/v1` (the commit used is in `internal/gen/SPEC_VERSION`); entity schemas and owners in
`api-specs/entities/restaurant`. Status of every operation: [docs/operations-status.md](docs/operations-status.md). Conservative
choices, open questions and spec problems: [docs/notes.md](docs/notes.md). No deployment exists yet (ADR-0001 AM12).

## Layout

```
cmd/catalog/            wiring only: serve, migrate
api/                    controllers: go-auth guard, strict JSON, Idempotency-Key, DTO mapping (no business rules)
service/                business rules: reads, projection of ops, tax-class edits, menu import; no HTTP, no SQL
repository/            interfaces; pg/ (Postgres, row-level security), mem/ (in memory), migrations/ (forward-only SQL)
events/                 domain events written to the transactional outbox
double/                 in-memory test double: the real api + service over repository/mem; loopback gateway; failure injection
internal/proj/          conflict policy of the 17 owned entities and the op decoder
internal/gen/          generated: oapi-codegen types and server, policy table, permission catalogue (make gen)
internal/seed/          the example worlds of the OpenAPI document (base, import-previewed, price-conflict)
internal/httpsuite/     HTTP suites run against both the real service and the double
internal/projsuite/     service-level projection and outlet-scope (T14) suites, run over memory and Postgres
```

## Run

Configuration is environment only; the process fails fast and lists every problem.

| Variable | Meaning |
|---|---|
| `CATALOG_DB_APP_DSN` | connects as `catalog_app` (required) |
| `CATALOG_DB_RELAY_DSN`, `NATS_URL`, `NATS_SEED_FILE` | outbox relay (`catalog_relay`) and the bus consumers; unset means events stay in the outbox and nothing is consumed |
| `CATALOG_DB_MIGRATE_DSN` | the migrator, used only by `catalog migrate` |
| `CATALOG_TENANT_JWKS` | JWKS document (URL or file) of the tenant token issuer (required) |
| `CATALOG_SERVICE_KEYS` | service key registry (JWKS, kid = service name); needed only to accept support sessions |
| `CATALOG_TAX_CLASSES` | tax classes an owner may choose (default `fnb_standard,fnb_non_gst,exempt`) |

DSNs and key files are secrets: pass them by reference from the secret store, never inline. Roles are provisioned once per
cluster by `scripts/provision-roles.sql`; `catalog migrate` then applies `repository/migrations`. At start-up the service refuses
to run as a superuser, a `BYPASSRLS` role or a table owner. The gateway emit client is not wired yet (docs/notes.md), so tax-class
edits and import commits answer 503 `emit_unavailable` in a real process.

```
catalog migrate     # as the migrator role
catalog serve       # the HTTP API, the outbox relay and the bus consumers when configured
```

## Test

```
make test           # unit, contract suite and HTTP suites on the double, lints (no database)
make race           # the same with -race
make dev-up         # Postgres from ../infra/dev, own compose project (catalog), host port 55440
make integration    # -tags integration: real Postgres and real service (contract, isolation, outlet scope, roles, inbox)
make dev-down
make lint           # vet, gofmt, golangci-lint when installed, outlet-scope lint (go-auth scopelint)
make gen            # regenerate internal/gen from ../api-specs-p2 and record the spec commit
```

Local development uses an uncommitted `go.work` listing `.`, `../go-common`, `../go-auth` and `../api-specs-p2`; CI uses the
pinned pseudo-versions in `go.mod`.

## Use the double

```go
env, _ := double.NewEnv()                    // trusts its own test issuer
env.SeedWorld(ctx, "base")                   // the example world of the spec
srv := env.Server()                          // httptest server; env.TenantToken(org) mints an owner token
env.Inject(double.Fault{Match: ..., Status: 503})   // failure injection
env.Emitted()                                // the ops catalog asked the gateway to record
```
