# Notes: conservative choices, open questions and spec problems

Decisions that were not ours to make are marked **ADR needed**; everything else is the conservative reading, stated so it can
be reversed cheaply. Phase 2 follow-up (api-specs 1462fb8, go-common c6c0fc7, ADR-0014 A2) closed the three ADR items of the first
round: the op event carries the whole op (read with `sync.ParseOp`), the gateway emit operation exists in `openapi/sync/v1`, and
`menu.tax_class.set` is in the intent registry.

## ADR needed

None open. Residuals worth a decision when the gateway is built:

1. **The gateway endpoint is not built.** Catalog is tested against an in-process fake of `POST /internal/v1/emit`
   (`internal/fakegw`, using the real go-common `emit` client and `emit_queue` worker). The process needs `CATALOG_SYNC_URL` and a
   service signing key (`CATALOG_SERVICE_SIGNING_KEY`, a PEM file by reference); how services obtain and rotate that key is
   ADR-0005's service-token matter, not decided here.

## Conservative choices

- **Projection** (go-common/sync engine, registry from api-specs `entities/restaurant`, embedded by `make gen`): per field group the
  highest `(hlc, origin, seq, index)` wins; price and tax class are `lww_review` groups. A void removes the refused op from every
  row it touched and a void that arrives first is remembered. Row ids are text. A mutation of an entity catalog does not own, of an
  entity on the wrong stream kind, or of an unknown entity is ignored (ADR-0002 S13). A malformed event is an error (retried, then
  parked); a quarantined op is never published, so there is no status to skip.
- **Price-conflict findings** are derived from the engine's conflicts of `paise` review fields: one per outlet of the brand known to
  catalog at detection time, closed as `superseded` when a later edit saw the winner. `resolved` (an op from a device,
  `conflict.confirm`) is not derived: the op's shape is not specified. `config_changed_shared_device` is not produced (no session
  type in an op body). Tax-class conflicts are resolved by the engine and need no finding.
- **X-Outlet-Id**: narrows the caller's scope for the permission; an outlet outside the scope, unknown or another tenant's is
  404. Absent: lists run over the whole scope; brand-wide data over the brands of the scope. `getMenu` needs exactly one outlet.
- **Archived**: rows are archived by tombstone; lists omit them unless `status=archived`; `getItem` reads them; `getMenu` omits them.
- **Ordering and cursors**: rows sort by `sort_order` then id; a cursor is the id of the last row; an unknown cursor id starts from
  the beginning (the shared example cursor names no row). Exceptions and import jobs are newest first.
- **Tax classes** an owner may choose come from configuration (`CATALOG_TAX_CLASSES`, default `fnb_standard,fnb_non_gst,exempt`).
  The rule set in force is billing's data and no read of it reaches catalog yet. No rate is here.
- **Emit** (ADR-0014 A2.2): a tax-class edit calls the client once with key `DeriveKey(operation|Idempotency-Key, 0)`; a down gateway
  is 503 `emit.unavailable` with Retry-After (nothing changed), a refusal is 500 `emit.rejected`. If the op landed but the request's
  transaction then failed, the retry reuses the key; because `prev` is re-read, a retry after the projection already changed the
  row would be an `idempotency.mismatch` at the gateway (a 500 `emit.rejected`); this window is the emit-first order of A2.2 item 6.
  `commitMenuImport` enqueues its ops in `emit_queue` in the commit's transaction (keys are UUIDv5 of job and position) and is
  accepted while the gateway is down; the response does not report emit progress (the spec's job has no field for it).
- **Import brand**: `brand_id`, else the brand of `X-Outlet-Id` (read from the header although the operation does not declare it, see
  spec problems), else the organisation's only brand, else 422 `brand_required`; no outlet yet is 422 `no_outlet`; a brand that is
  not one of the caller's outlets' brands is 404. Re-import matches by item code only and updates names and prices (a matched
  variant is found by name; a new variant name is added); items without a code never match. An unchanged name is not re-set.
  New items take the category's tax class, new categories `fnb_standard`. An existing price row is edited with `prev` (RM-M4).
- **Support import may only add** (security review, ADR-0005 A2.5 item 5): a preview from a support session that would rename an
  existing item or change an existing price is 422 `owner_commit_required` and stores nothing; new categories, items and variants
  (and a price repeated unchanged) are fine. The emitted ops name `catalog.import` as the permission, an owner's `menu.import`.
- **Vendor audit**: a support-token request writes `vendor_access_log` in its transaction (go-auth `WithAuditedTx`), and a refused
  one in its own. Catalog publishes no event for these rows (no `catalog.vendor_access.recorded` schema exists). Support tokens
  need the admin service token and a configured service key registry (`CATALOG_SERVICE_KEYS`); without it they are refused.
- **Strong session wording**: go-auth (feat/phase1-go-auth, 7551373) still answers every ceiling with `auth.permission_denied`, and has
  no `strong_session_required` code; the api layer keeps rewriting that one answer to `strong_session_required` for pin and device
  tokens, as the contract states. It never grants anything. Drop the rewrite when go-auth has the code.
- **Timestamps** are written with exactly three fractional digits, as the examples show.
- **Idempotency**: key, operation and fingerprint are stored in the same transaction as the effect; the same key with another
  body is 422 `idempotency.mismatch`.
- **Migration 002** replaces the `ops` and `effects` tables with `sync_candidates` and `sync_voids` (go-common schema, text tenant
  with the standard policy) and adds `emit_queue`, drained by the `catalog_relay` role. The old tables are dropped in the same
  release because nothing was ever deployed (ADR-0001 AM12); once something is, such a change needs expand/contract.
- **Example worlds**: `menu-empty` and `import-previewed` hold no menu rows; the id minted by a creating operation of a world is the
  one its example shows (`seed.IDs`: the import job of `menu-empty` is 5171), and the double's clock stands four minutes after the
  preview in `import-previewed` (the commit example's `committed_at`). `base-with-import` is a test-only world.

## Spec problems (api-specs, feat/phase2-specs; not edited here)

1. `createMenuImport` description gives `X-Outlet-Id` a meaning ("the brand of the outlet") but the operation declares no such
   parameter, so the generated handler has none; catalog reads the header directly.
2. go-auth does not implement `x-permission-widened-by` (catalog.import widening menu.import): a support act-mode token does not pass
   the guard of the import operations, so the add-only rule cannot be exercised end to end yet (it is tested at the service).
3. `strong_session_required` is declared by strong-only operations but go-auth has no such code (see above).
4. The permission catalogue gives `settings.read` to tenancy and `exceptions.view` to orders; catalog uses both as declared.

Resolved by 1462fb8: the `OutletScope` example id, the createMenuImport and commit examples, the tax-class `op_id` example, the
`next_cursor` and `available=false` examples, the missing brand input, the UUIDv5 price id, and catalog as subscriber of the
tenancy outlet events and the sync op events. The contract tests no longer patch a temporary copy of the spec.

## Not done, by design

- No write API for synced masters: devices write them as ops (ADR-0014 R1).
- No analytics, no reports.
- Deployment is deferred (ADR-0001 AM12); migrations and roles run locally and in CI only.
- `go.mod` still pins the earlier api-specs, go-auth and go-common pseudo-versions; the development workspace (`go.work`, not
  committed) resolves the Phase 2 branches. Pin the new commits and tidy once those branches are tagged or merged.
