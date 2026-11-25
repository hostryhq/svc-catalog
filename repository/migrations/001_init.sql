-- Catalog service schema, first migration (forward-only). Runs as the migrator role, which owns every table;
-- the runtime roles must already exist (cluster provisioning, see scripts/provision-roles.sql):
--   catalog_app    DML only, NOSUPERUSER NOBYPASSRLS, not an owner (the service's runtime role)
--   catalog_relay  outbox relay: the outbox table only (ADR-0003 section 6)
--
-- Table classes (ADR-0003 section 2): tenant-owned (standard policy), outlet-scoped (carry outlet_id and are
-- always read with the caller's outlet scope, ADR-0003 A1.4), service-internal (outbox, idempotency_keys,
-- event_keys, inbox, event_parked). There are no global tenant-owned tables.

-- outlets as projected from tenancy's outlet events: which brand (menu) an outlet sells
CREATE TABLE outlets (
    tenant_id uuid NOT NULL,
    outlet_id uuid NOT NULL,
    brand_id  uuid NOT NULL,
    kind      text NOT NULL,
    status    text NOT NULL,
    PRIMARY KEY (tenant_id, outlet_id)
);

-- the op log of the projection: idempotent apply and the void set (a void may arrive before its op)
CREATE TABLE ops (
    tenant_id  uuid        NOT NULL,
    op_key     text        NOT NULL,
    stream     text        NOT NULL,
    intent     text        NOT NULL,
    voided     boolean     NOT NULL DEFAULT false,
    applied_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, op_key)
);

-- every mutation of every row, kept so the visible row is a pure function of the set of ops (ADR-0002 S5)
CREATE TABLE effects (
    tenant_id  uuid   NOT NULL,
    entity     text   NOT NULL,
    row_id     text   NOT NULL,
    op_key     text   NOT NULL,
    idx        int    NOT NULL,
    stream     text   NOT NULL,
    scope_id   uuid   NOT NULL,
    hlc        bigint NOT NULL,
    origin     text   NOT NULL,
    seq        bigint NOT NULL,
    kind       text   NOT NULL,
    fields     jsonb  NOT NULL,
    prev       jsonb  NOT NULL,
    PRIMARY KEY (tenant_id, entity, row_id, op_key, idx)
);
CREATE INDEX effects_by_op ON effects (tenant_id, op_key);

-- projected rows of the brand stream (menu): scope is the brand
CREATE TABLE brand_rows (
    tenant_id  uuid        NOT NULL,
    brand_id   uuid        NOT NULL,
    entity     text        NOT NULL,
    row_id     text        NOT NULL,
    sort_key   bigint      NOT NULL DEFAULT 0,
    gone       boolean     NOT NULL DEFAULT false,
    fields     jsonb       NOT NULL,
    stamps     jsonb       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, entity, row_id)
);
CREATE INDEX brand_rows_list ON brand_rows (tenant_id, entity, brand_id, gone, sort_key, row_id);

-- projected rows of the outlet stream (set-up, overlay): outlet-scoped
CREATE TABLE outlet_rows (
    tenant_id  uuid        NOT NULL,
    outlet_id  uuid        NOT NULL,
    entity     text        NOT NULL,
    row_id     text        NOT NULL,
    sort_key   bigint      NOT NULL DEFAULT 0,
    gone       boolean     NOT NULL DEFAULT false,
    fields     jsonb       NOT NULL,
    stamps     jsonb       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, entity, row_id)
);
CREATE INDEX outlet_rows_list ON outlet_rows (tenant_id, entity, outlet_id, gone, sort_key, row_id);

-- detector findings, one per conflicting write and outlet (outlet-scoped)
CREATE TABLE exceptions (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL,
    outlet_id    uuid        NOT NULL,
    dedupe_key   text        NOT NULL,
    kind         text        NOT NULL,
    severity     text        NOT NULL,
    status       text        NOT NULL CHECK (status IN ('open', 'resolved', 'superseded')),
    subject_kind text        NOT NULL,
    subject_id   text        NOT NULL,
    detected_at  timestamptz NOT NULL,
    resolved_at  timestamptz,
    resolved_ref text        NOT NULL DEFAULT '',
    detail_key   text        NOT NULL,
    params       jsonb       NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, dedupe_key)
);
CREATE INDEX exceptions_list ON exceptions (tenant_id, outlet_id, detected_at DESC, id DESC);

-- menu import jobs (server-only data, ADR-0014 R3); the frozen plan is cleared once the job ends
CREATE TABLE menu_imports (
    tenant_id          uuid        NOT NULL,
    id                 uuid        NOT NULL,
    status             text        NOT NULL CHECK (status IN ('previewed', 'committed', 'cancelled')),
    source             text        NOT NULL CHECK (source IN ('owner', 'support_session')),
    filename           text        NOT NULL,
    row_count          int         NOT NULL,
    error_count        int         NOT NULL,
    errors             jsonb       NOT NULL,
    categories_created int         NOT NULL,
    items_created      int         NOT NULL,
    items_updated      int         NOT NULL,
    price_changes      int         NOT NULL,
    created_at         timestamptz NOT NULL,
    committed_at       timestamptz,
    created_by         text        NOT NULL DEFAULT '',
    plan               jsonb,
    PRIMARY KEY (tenant_id, id)
);

-- who, what, when, before and after of strong-session actions
CREATE TABLE audit_log (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL,
    at           timestamptz NOT NULL,
    actor        text        NOT NULL,
    action       text        NOT NULL,
    subject_kind text        NOT NULL,
    subject_id   text        NOT NULL,
    before       text        NOT NULL DEFAULT '',
    after        text        NOT NULL DEFAULT '',
    reason       text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- vendor_access_log: rows written for support-token requests, in the transaction of the request (ADR-0013 V5)
CREATE TABLE vendor_access_log (
    tenant_id          uuid        NOT NULL,
    id                 uuid        NOT NULL,
    occurred_at        timestamptz NOT NULL,
    vendor_user        text        NOT NULL,
    token_type         text        NOT NULL,
    support_session_id text,
    action_key         text        NOT NULL,
    target_id          text,
    ticket_reference   text,
    outcome            text        NOT NULL,
    result_count       int         NOT NULL,
    request_id         text        NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE idempotency_keys (
    tenant_id   uuid        NOT NULL,
    op          text        NOT NULL,
    key         text        NOT NULL,
    fingerprint text        NOT NULL,
    response    bytea       NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, op, key)
);

CREATE TABLE event_keys (
    tenant_id uuid   NOT NULL,
    key       text   NOT NULL,
    last_seq  bigint NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

-- transactional outbox (go-common outbox.Schema), consumer inbox and parked events (go-common inbox.Schema)
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    type         text        NOT NULL,
    subject      text        NOT NULL,
    tenant_id    text        NOT NULL,
    key          text        NOT NULL DEFAULT '',
    key_seq      bigint      NOT NULL DEFAULT 0,
    payload      bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_published_at ON outbox (published_at) WHERE published_at IS NOT NULL;

CREATE TABLE outbox_relay_state (
    stream     text        PRIMARY KEY,
    generation text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE inbox (
    consumer     text        NOT NULL,
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE event_parked (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    consumer    text        NOT NULL,
    event_id    text        NOT NULL,
    subject     text        NOT NULL,
    payload     bytea       NOT NULL,
    error       text        NOT NULL,
    attempts    bigint      NOT NULL,
    parked_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

-- ---------------------------------------------------------------- row-level security (ADR-0003 section 6)
-- Standard policy on every tenant-owned and service-internal table: enabled, forced, fail closed.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['outlets', 'ops', 'effects', 'brand_rows', 'outlet_rows', 'exceptions', 'menu_imports', 'audit_log',
                             'vendor_access_log', 'idempotency_keys', 'event_keys']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- The outbox carries tenant_id as text: the same policy for the application role, plus the relay role.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox TO catalog_app
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY relay_all ON outbox TO catalog_relay USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------- privileges
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON outlets, ops, effects, brand_rows, outlet_rows, exceptions, idempotency_keys, event_keys TO catalog_app;
GRANT SELECT, INSERT, UPDATE ON menu_imports TO catalog_app;
GRANT SELECT, INSERT ON audit_log, vendor_access_log, outbox, inbox, event_parked TO catalog_app;
GRANT UPDATE ON event_parked TO catalog_app;
GRANT SELECT, UPDATE, DELETE ON outbox TO catalog_relay;
GRANT SELECT, INSERT, UPDATE ON outbox_relay_state TO catalog_relay;
GRANT DELETE ON inbox TO catalog_app;
