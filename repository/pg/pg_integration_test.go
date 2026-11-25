//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/pgtest"
	"github.com/hostryhq/svc-catalog/internal/projsuite"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/repository/pg"
	"github.com/hostryhq/svc-catalog/service"
)

func newPG(t *testing.T) (*service.Service, *pgtest.DB) {
	db := pgtest.New(t)
	return service.New(pg.New(db.App), service.Options{Log: slog.New(slog.DiscardHandler)}), db
}

// The same projection suites the unit tests run over memory, over Postgres.
func TestProjectionPostgres(t *testing.T) {
	projsuite.Run(t, func(t *testing.T) *service.Service { svc, _ := newPG(t); return svc })
}

func TestRuntimeRoleChecks(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if err := pg.VerifyRuntimeRole(ctx, db.App); err != nil {
		t.Fatalf("the application role must pass: %v", err)
	}
	if err := pg.VerifyRuntimeRole(ctx, db.Admin); err == nil {
		t.Fatal("a superuser pool must be refused")
	}
	if err := pg.VerifyRuntimeRole(ctx, db.Migrator); err == nil {
		t.Fatal("a table owner must be refused")
	}
}

// Row-level security holds even for a statement with no tenant predicate (ADR-0003 section 6).
func TestRowLevelSecurityFailsClosed(t *testing.T) {
	svc, db := newPG(t)
	ctx := context.Background()
	for _, w := range []seed.World{seed.Example, seed.Other} {
		if err := w.Apply(ctx, svc, seed.Base); err != nil {
			t.Fatal(err)
		}
	}
	count := func(tenant ids.ID, table string) int {
		tx, err := db.App.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if tenant != "" {
			if err := tenancy.SetDBContext(ctx, tx, tenant); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		//nolint:gosec // fixed table names of this test
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil { // no tenant predicate on purpose
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"brand_rows", "outlet_rows", "sync_candidates", "outlets"} {
		all, one, none := count(seed.Example.Org(), table), count(seed.Other.Org(), table), count("", table)
		if none != 0 {
			t.Errorf("%s: no tenant context must see no row, saw %d", table, none)
		}
		if all == 0 || one == 0 {
			t.Errorf("%s: each tenant must see its own rows (%d, %d)", table, all, one)
		}
	}
	// A write for another tenant is refused by the policy's WITH CHECK.
	tx, _ := db.App.BeginTx(ctx, nil)
	defer func() { _ = tx.Rollback() }()
	_ = tenancy.SetDBContext(ctx, tx, seed.Example.Org())
	if _, err := tx.ExecContext(ctx, `INSERT INTO outlets (tenant_id, outlet_id, brand_id, kind, status) VALUES ($1,$2,$3,'restaurant','active')`,
		seed.Other.Org(), ids.New(), ids.New()); err == nil {
		t.Fatal("inserting a row for another tenant must fail")
	}
}

// emit_queue holds every tenant's queued ops, so the application role sees only its own tenant's rows and cannot
// change them, while the relay role (the worker) sees and updates all of them (migration 002).
func TestEmitQueueRoles(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	a, b := seed.Example.Org(), seed.Other.Org()
	perm := "menu.import"
	op := emit.Op{Key: emit.DeterministicKey("test", "1"), Stream: "brand:" + seed.Example.Brand().String(), Intent: "menu.import.apply", IntentV: 1, Actor: "member-1", AuthorisedBy: &perm,
		Muts: []gsync.Mut{{E: "category", ID: seed.Example.Category().String(), K: gsync.KindInsert, F: map[string]any{"name_en": "Starters"}}}}
	tx, err := db.App.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tenancy.SetDBContext(ctx, tx, a); err != nil {
		t.Fatal(err)
	}
	if err := emit.Enqueue(ctx, tx, time.Now(), a, op, op); err != nil { // the second is the same key: ignored
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	seen := func(d *sql.DB, tenant ids.ID) int {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if tenant != "" {
			_ = tenancy.SetDBContext(ctx, tx, tenant)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM emit_queue`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := seen(db.App, a); n != 1 {
		t.Fatalf("the enqueuing tenant sees its one row, saw %d", n)
	}
	if n, m := seen(db.App, b), seen(db.App, ""); n != 0 || m != 0 {
		t.Fatalf("another tenant and no tenant see nothing: %d %d", n, m)
	}
	if n := seen(db.Relay, ""); n != 1 {
		t.Fatalf("the worker's role sees the row of every tenant, saw %d", n)
	}
	tx, _ = db.App.BeginTx(ctx, nil)
	defer func() { _ = tx.Rollback() }()
	_ = tenancy.SetDBContext(ctx, tx, a)
	if _, err := tx.ExecContext(ctx, `UPDATE emit_queue SET state = 'sent'`); err == nil {
		t.Fatal("the application role must not change queue rows")
	}
}

type delivery struct {
	data  []byte
	acked bool
	naked bool
}

func (d *delivery) Subject() string         { return "evt.sync.op.stored.v1" }
func (d *delivery) Data() []byte            { return d.data }
func (d *delivery) NumDelivered() uint64    { return 1 }
func (d *delivery) Ack() error              { d.acked = true; return nil }
func (d *delivery) Nak(time.Duration) error { d.naked = true; return nil }

// The inbox path: the projection commits with the inbox row, a redelivery is acknowledged without effect, and
// an event nobody can apply is retried (not lost).
func TestConsumerThroughTheInbox(t *testing.T) {
	svc, db := newPG(t)
	_ = svc
	ctx := context.Background()
	w := seed.Example
	handle := func(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
		rtx, err := pg.Bind(ctx, tx, env.TenantID)
		if err != nil {
			return err
		}
		return svc.ApplyEvent(ctx, rtx, env)
	}
	sub := &inbox.Subscriber{DB: db.App, Consumer: "catalog:test", Handle: handle, Log: slog.New(slog.DiscardHandler)}
	d, _ := json.Marshal(map[string]any{"outlet_id": w.Outlet(), "brand_id": w.Brand(), "kind": "restaurant", "status": "active"})
	env := events.Envelope{ID: ids.New(), Type: service.EventOutletCreated, V: 1, Source: "tenancy", TenantID: w.Org(), OutletID: w.Outlet(), Key: w.Outlet().String(), OccurredAt: seed.At, Data: d}
	raw, _ := json.Marshal(env)
	for i := 0; i < 2; i++ { // the second is a redelivery of the same event id
		dl := &delivery{data: raw}
		if err := sub.Process(ctx, dl); err != nil || !dl.acked {
			t.Fatalf("delivery %d: err %v acked %v", i, err, dl.acked)
		}
	}
	var n int
	if err := db.Admin.QueryRowContext(ctx, `SELECT count(*) FROM outlets`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("one outlet projected: %d %v", n, err)
	}
	bodyless, _ := json.Marshal(events.Envelope{ID: ids.New(), Type: "sync.op.stored", V: 1, Source: "sync", TenantID: w.Org(), Key: "k", OccurredAt: seed.At,
		Data: json.RawMessage(`{"origin":"d","stream":"brand:` + w.Brand().String() + `","seq":1,"status":"accepted","intent":"menu.item.edit"}`)})
	dl := &delivery{data: bodyless}
	if err := sub.Process(ctx, dl); err != nil || !dl.naked || dl.acked {
		t.Fatalf("an op event without a body must be retried, not acknowledged: err %v naked %v acked %v", err, dl.naked, dl.acked)
	}
}
