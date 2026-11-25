# Operations status

Every operationId of `api-specs/openapi/catalog/v1/openapi.yaml` (commit recorded in `internal/gen/SPEC_VERSION`).
Status is **implemented** in the service (`api` -> `service` -> `repository/pg`), in the test double (`double/`, same API and
service over `repository/mem`) and covered by the shared contract suite (`contracttest`) in the example world the spec declares for it
(`SeedWorld`, `internal/seed`), on the real service (Postgres) and on the double, with no operation skipped.
**Not yet implemented: none.** The spec has 19 operations (the work package said 17).

`C` = example-driven contract suite (real service and double). `H` = HTTP suites run on both (`internal/httpsuite`).
The double additionally serves the spec's own response example for every operation except the six listed under
"Examples the double cannot reproduce" (`double/contract_test.go`); those still pass the schema, status and replay checks.

| operationId | Method and path | Permission | Status | Contract | Notes |
|---|---|---|---|---|---|
| listCategories | GET /v1/categories | menu.read | implemented | C | ordered by sort order; archived only with `status=archived`; brand data of the caller's outlets |
| setCategoryTaxClass | PUT /v1/categories/{category_id}/tax-class | menu.tax_class.edit (step-up, strong only) | implemented | C, H | one `set` op on the brand stream through the emitter, citing `prev`; audited; 403 `strong_session_required` in pin and device sessions; 503 `emit_unavailable` without a gateway |
| listDiningTables | GET /v1/dining-tables | settings.read | implemented | C, H | outlet scoped (T14) |
| listCatalogExceptions | GET /v1/exceptions | exceptions.view | implemented | C, H | outlet scoped; `price_conflict` only (see notes) |
| listFloors | GET /v1/floors | settings.read | implemented | C, H | outlet scoped (T14) |
| listItemAvailability | GET /v1/item-availability | menu.read | implemented | C, H | outlet scoped (T14); `available` filter treats an absent flag as available |
| listItems | GET /v1/items | menu.read | implemented | C, H | variants with prices, modifier group links; `q` = name in any language or exact code; filters by category and status |
| getItem | GET /v1/items/{item_id} | menu.read | implemented | C, H | archived items stay readable by id |
| setItemTaxClass | PUT /v1/items/{item_id}/tax-class | menu.tax_class.edit (step-up, strong only) | implemented | C, H | as setCategoryTaxClass |
| getMenu | GET /v1/menu | menu.read | implemented | C, H | needs one outlet: `X-Outlet-Id`, or a caller scoped to exactly one; archived excluded |
| listMenuImports | GET /v1/menu-imports | menu.import (step-up, strong only) | implemented | C, H | newest first |
| createMenuImport | POST /v1/menu-imports | menu.import (step-up, strong only) | implemented | C, H | row validation, preview counts, frozen plan; max 2,000 rows; targets the organisation's one brand |
| getMenuImport | GET /v1/menu-imports/{import_id} | menu.import (step-up, strong only) | implemented | C, H | |
| cancelMenuImport | POST /v1/menu-imports/{import_id}/cancellation | menu.import (step-up, strong only) | implemented | C, H | a repeat is a no-op; 409 `import_not_previewed` once committed |
| commitMenuImport | POST /v1/menu-imports/{import_id}/commit | menu.import (step-up, strong only) | implemented | C, H | all rows or none; ops keyed from the job id; publishes `catalog.menu_import.committed`; audited |
| listModifierGroups | GET /v1/modifier-groups | menu.read | implemented | C, H | groups with their options |
| listPrinters | GET /v1/printers | settings.read | implemented | C, H | outlet scoped (T14) |
| listReasonCodes | GET /v1/reason-codes | menu.read | implemented | C, H | filter by purpose |
| listStations | GET /v1/stations | settings.read | implemented | C, H | outlet scoped (T14) |

19 of 19 operations implemented.

## Not an operation: the projection

Catalog's data arrives as ops. `service.ApplyEvent` consumes `sync.op.*` events (body of the op, accepted or flagged
status) and `tenancy.outlet.created|updated`; `service.ApplyOp` applies one op (idempotent, order independent, voids,
conflict policy of ADR-0002 S6 per field, 17 owned entities). Postgres runs it in the inbox transaction (`internal/app`
`Consume`, `repository/pg.Bind`).

## Examples the double cannot reproduce (spec problems, see docs/notes.md)

createMenuImport (counts), commitMenuImport (`committed_at`), setCategoryTaxClass and setItemTaxClass (`op_id`),
listItems (`next_cursor` on a last page), listItemAvailability (query `available=false` but an available row).

## Known limits

- Cloud-side writes (tax class, import commit) need the gateway emit API, which is not in api-specs; the process runs
  without an emitter and answers 503 `emit_unavailable` until it is wired (docs/notes.md, ADR needed).
- Support act mode (`catalog.import`) is dormant: identity answers `mode_not_supported`, and go-auth refuses a support token
  on a strong-only permission. The `source` mapping (`support_session`) is in place.
- `config_changed_shared_device` exceptions are not produced: the op body carries no session type.
