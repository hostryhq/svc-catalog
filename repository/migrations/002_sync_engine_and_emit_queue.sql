-- Phase 2 follow-up (forward-only): the projection moves to the go-common/sync engine and cloud-origin writes go
-- through the emit_queue outbox (ADR-0014 A2.1, A2.2).
--
-- sync_candidates / sync_voids are go-common sync.Schema (insert-only candidates, the void set); catalog owns no
-- entity with a personal-data field, so sync_pii is not created. emit_queue is go-common emit.QueueSchema.
-- The earlier `ops` and `effects` tables are replaced by candidates and voids. Nothing was ever deployed
-- (ADR-0001 AM12), so they are dropped in this release rather than kept for one more.

DROP TABLE ops;
DROP TABLE effects;

CREATE TABLE sync_candidates (
  tenant_id text   NOT NULL,
  entity    text   NOT NULL,
  row_id    text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  idx       int    NOT NULL,
  hlc       text   NOT NULL,
  kind      text   NOT NULL,
  fields    jsonb  NOT NULL DEFAULT '{}',
  prev      jsonb  NOT NULL DEFAULT '{}',
  PRIMARY KEY (tenant_id, entity, row_id, origin, stream, seq, idx)
);
CREATE INDEX sync_candidates_op ON sync_candidates (tenant_id, origin, stream, seq);
CREATE TABLE sync_voids (
  tenant_id text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  PRIMARY KEY (tenant_id, origin, stream, seq)
);

CREATE TABLE emit_queue (
  id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id       text        NOT NULL,
  key             text        NOT NULL,
  op              jsonb       NOT NULL,
  state           text        NOT NULL DEFAULT 'pending',
  attempts        int         NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  last_error      text        NOT NULL DEFAULT '',
  origin          text,
  stream          text,
  seq             bigint,
  replayed        boolean,
  created_at      timestamptz NOT NULL,
  sent_at         timestamptz,
  UNIQUE (tenant_id, key)
);
CREATE INDEX emit_queue_pending ON emit_queue (tenant_id, id) WHERE state = 'pending';

-- Row-level security. tenant_id is text here (as in go-common's schemas and the outbox): the same policy as the
-- other tenant tables for the application role. The emit worker drains every tenant, so it runs as catalog_relay,
-- which sees all rows of emit_queue and nothing else tenant-owned (ADR-0003 section 6).
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['sync_candidates', 'sync_voids', 'emit_queue']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I TO catalog_app
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t);
    END LOOP;
END $$;
CREATE POLICY relay_all ON emit_queue TO catalog_relay USING (true) WITH CHECK (true);

GRANT SELECT, INSERT ON sync_candidates, sync_voids TO catalog_app;
GRANT SELECT, INSERT ON emit_queue TO catalog_app;
GRANT SELECT, UPDATE ON emit_queue TO catalog_relay;
