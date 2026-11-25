# Notes: conservative choices, open questions and spec problems

Decisions that were not ours to make are marked **ADR needed**; everything else is the conservative reading, stated so it can
be reversed cheaply.

## ADR needed

1. **Sync op events do not carry the op.** svc-sync publishes `sync.op.stored` with `{origin, stream, seq, h, intent, status}`
   and no body; ADR-0011 B10 and ADR-0002 A2.3 leave the final event name and payload open. A projecting service needs the op
   body (the mutations). Catalog accepts `data.body` (canonical JSON text, byte for byte) and otherwise asks an `OpBodyFetcher`
   (a gateway read, not defined). With neither, the event is retried and parked (`ErrOpBodyUnavailable`). Decide: body in the
   event, or a gateway read API (and add `catalog` to the subscribers; see spec problems).
2. **Gateway emit API contract.** ADR-0014 R1 says services call `emit`; svc-sync has `Gateway.Emit` in Go only. Catalog defines
   `service.Emitter` (stream, intent, idempotency key, mutations; returns `origin/stream/seq`) and ships a loopback in
   `double/`. No HTTP client is wired in `cmd/catalog` (cloud writes answer 503). Needs an `api-specs` operation, the service-token
   caller list, and whether the gateway de-duplicates on a client key (catalog derives keys from the import job id, and from the
   `Idempotency-Key` for tax-class edits).
3. **Intent for strong-only cloud edits.** `sync/intents` has `menu.import.apply` (used) but no intent for an online tax-class
   edit; `cloud.emit` lists no catalog entity. Catalog emits `menu.tax_class.set` (provisional). Add it to the registry
   (class config, device kind cloud, streams brand, entities item and category: set).

## Conservative choices

- **Projection**: effects are kept per row and the row is derived from them (pure function of the op set, so arrival order,
  replays and re-offers converge). A void removes the refused op's effects and re-derives; a void that arrives first is
  remembered. Row ids are text (UUIDv5 ids exist). A mutation of an entity catalog does not own, of a stream kind it does not
  belong to, or of an unknown kind is ignored (ADR-0002 S13). Field groups are single fields (price and tax class each win alone).
- **X-Outlet-Id**: narrows the caller's scope for the permission; an outlet outside the scope, unknown or another tenant's is
  404. Absent: lists run over the whole scope; brand-wide data over the brands of the scope (an organisation-wide scope with no
  header reaches every brand). `getMenu` needs exactly one outlet.
- **Archived**: rows are archived by tombstone; lists omit them unless `status=archived`; `getItem` reads them; `getMenu` omits them.
- **Ordering and cursors**: rows sort by `sort_order` then id; a cursor is the id of the last row; an unknown cursor id starts from
  the beginning (the shared example cursor names no row). Exceptions and import jobs are newest first.
- **Tax classes** an owner may choose come from configuration (`CATALOG_TAX_CLASSES`, default `fnb_standard,fnb_non_gst,exempt`).
  The rule set in force is billing's data and no read of it reaches catalog yet. Hostry still publishes rates; no rate is here.
- **Import brand**: the API names none. The organisation's brand known from its outlets is used; several brands is 422
  `brand_ambiguous`, no outlet yet is 422 `no_outlet`. Re-import matches by item code only and updates names and prices (a
  matched variant is found by name; a new variant name is added); items without a code never match. New items take the category's
  tax class, new categories `fnb_standard`. An existing price row is edited with `prev` (a price edit, RM-M4).
- **Price conflict exceptions**: a brand-stream finding is stored once per outlet of the brand known to catalog at detection time;
  it closes as `superseded` when a later edit saw the winner. `resolved` (an op from a device, `conflict.confirm`) is not
  derived: the op's shape is not specified. `config_changed_shared_device` is not produced (no session type in an op body).
- **Vendor audit**: a support-token request writes `vendor_access_log` in its transaction (go-auth `WithAuditedTx`), and a refused
  one in its own. Catalog publishes no event for these rows (no `catalog.vendor_access.recorded` schema exists). Support tokens
  need the admin service token and a configured service key registry (`CATALOG_SERVICE_KEYS`); without it they are refused.
- **Strong session wording**: go-auth answers every ceiling with `auth.permission_denied`; the api layer rewrites that one answer to
  `strong_session_required` for pin and device tokens, as the contract states. It never grants anything.
- **Timestamps** are written with exactly three fractional digits, as the examples show.
- **Idempotency**: key, operation and fingerprint are stored in the same transaction as the effect; the same key with another
  body is 422 `idempotency.mismatch`. An emitted op is not undone if the transaction fails afterwards; the retry reuses the same
  emission key.

## Spec problems (api-specs, feat/phase2-specs; not edited here)

1. `common` parameter `OutletScope` carries example `...4a5c`, an id no entity uses; every catalog example is about outlet `...4a61`.
   The contract tests patch it in a temporary copy (`internal/contractenv`).
2. createMenuImport example: the base world already holds category Starters and item code 101, so the example CSV is an update, yet
   the response says `categories_created: 1, items_created: 1`.
3. commitMenuImport example `committed_at` (13:09:40) and the tax-class `op_id` (`cloud-1/brand:0190a1b2/412`, a truncated id) cannot
   be produced by a deterministic double; listItems shows a `next_cursor` on a last page; listItemAvailability sends
   `available=false` but shows an available row.
4. `createMenuImport` has no brand (or outlet) input although items and categories are brand streams.
5. `catalog` is not an `x-subscriber` of `tenancy.outlet.created|updated` (needed for outlet to brand) nor of any sync op event
   (no `sync.*` schema in `events/`).
6. strong-only operations declare `strong_session_required` but go-auth has no such code (see above).
7. `price_entry` example id `...5191` is not the UUIDv5 the entity file prescribes; the seed uses it as is, and import edits the
   existing row by (item, variant) rather than recomputing the id.
8. The permission catalogue gives `settings.read` to tenancy and `exceptions.view` to orders; catalog uses both as declared.

## Not done, by design

- No write API for synced masters: devices write them as ops (ADR-0014 R1).
- No analytics, no reports.
- Deployment is deferred (ADR-0001 AM12); migrations and roles run locally and in CI only.
