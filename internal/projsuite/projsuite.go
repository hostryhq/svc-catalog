// Package projsuite holds the service-level suites of the projection (ADR-0002 S5, S6): idempotent and
// order-independent apply, voids, review conflicts, outlet scoping and the event adapters. They run over the
// in-memory repository (unit) and over Postgres (integration) so the two cannot drift.
package projsuite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/service"
)

// Factory builds a service over an empty store.
type Factory func(t *testing.T) *service.Service

var ctxBg = context.Background()

func catalogue(t testing.TB) *auth.Catalogue {
	t.Helper()
	c, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// As returns a context for tenant with the given grants (owner when none).
func As(t testing.TB, tenant ids.ID, grants ...auth.Grant) context.Context {
	t.Helper()
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	iss := authtest.NewTenantIssuer(testkit.NewClock())
	return iss.ContextFor(ctxBg, catalogue(t), authtest.Spec{Tenant: tenant, Grants: grants})
}

func hlc(ms uint64, counter uint64) uint64 { return ms<<16 + counter }

func op(origin, stream string, seq int64, h uint64, muts ...proj.Mut) proj.Op {
	return proj.Op{Ref: proj.Ref{Origin: origin, Stream: stream, Seq: seq}, HLC: h, Intent: "menu.item.edit", Muts: muts}
}

func itemNames(t *testing.T, svc *service.Service, ctx context.Context) map[string]string {
	t.Helper()
	pg, err := svc.ListItems(ctx, service.ListItemsInput{ListInput: service.ListInput{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, i := range pg.Items {
		out[i.ID] = i.NameEn
	}
	return out
}

func outletEvent(tenant, outlet, brand ids.ID) events.Envelope {
	d, _ := json.Marshal(map[string]any{"outlet_id": outlet, "brand_id": brand, "kind": "restaurant", "status": "active"})
	return events.Envelope{ID: ids.New(), Type: service.EventOutletCreated, V: 1, Source: "tenancy", TenantID: tenant, Key: outlet.String(), OccurredAt: seed.At, Data: d}
}

// Run executes every suite.
func Run(t *testing.T, mk Factory) {
	t.Run("apply is idempotent and order independent", func(t *testing.T) { applyOrder(t, mk) })
	t.Run("a void removes the refused op and keeps later edits", func(t *testing.T) { voids(t, mk) })
	t.Run("a void that arrives before its op wins", func(t *testing.T) { voidFirst(t, mk) })
	t.Run("concurrent price edits raise one exception", func(t *testing.T) { priceConflict(t, mk) })
	t.Run("ignored mutations and events", func(t *testing.T) { ignored(t, mk) })
	t.Run("sync op events", func(t *testing.T) { opEvents(t, mk) })
	t.Run("projection never crosses tenants", func(t *testing.T) { tenants(t, mk) })
	t.Run("outlet scope (T14)", func(t *testing.T) { T14(t, mk) })
}

var (
	brandA  = ids.ID("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6a")
	outletA = ids.ID("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a61")
	catID   = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5001"
	item1   = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5101"
	item2   = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5102"
	price1  = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5191"
)

func insItem(id, name string) proj.Mut {
	return proj.Mut{E: "item", ID: id, K: proj.KInsert, F: map[string]any{"category_id": catID, "name_en": name, "kitchen_name": name, "veg_mark": "veg",
		"tax_class": "fnb_standard", "status": "active", "sort_order": int64(1)}}
}

func setup(t *testing.T, svc *service.Service, tenant ids.ID) {
	t.Helper()
	if err := svc.ApplyEventTx(ctxBg, outletEvent(tenant, outletA, brandA), nil); err != nil {
		t.Fatal(err)
	}
}

func applyOrder(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	ops := []proj.Op{
		op("d1", stream, 1, hlc(1000, 0), insItem(item1, "Tea")),
		op("d2", stream, 1, hlc(1001, 0), proj.Mut{E: "item", ID: item1, K: proj.KSet, F: map[string]any{"name_en": "Chai"}}),
		op("d1", stream, 2, hlc(1002, 0), insItem(item2, "Coffee")),
	}
	for i := len(ops) - 1; i >= 0; i-- { // newest first: sets arrive before the insert they belong to
		if err := svc.ApplyOpTx(ctxBg, tenant, ops[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range ops { // a replay of every op changes nothing
		if err := svc.ApplyOpTx(ctxBg, tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	got := itemNames(t, svc, As(t, tenant))
	if got[item1] != "Chai" || got[item2] != "Coffee" || len(got) != 2 {
		t.Fatalf("converged state wrong: %v", got)
	}
}

func voids(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	apply := func(o proj.Op) {
		t.Helper()
		if err := svc.ApplyOpTx(ctxBg, tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	apply(op("d1", stream, 1, hlc(1000, 0), insItem(item1, "Tea")))
	apply(op("d2", stream, 1, hlc(1005, 0), proj.Mut{E: "item", ID: item1, K: proj.KSet, F: map[string]any{"name_en": "Refused edit"}}))
	apply(op("d3", stream, 1, hlc(1003, 0), proj.Mut{E: "item", ID: item1, K: proj.KSet, F: map[string]any{"name_en": "Legit edit"}}))
	if got := itemNames(t, svc, As(t, tenant))[item1]; got != "Refused edit" {
		t.Fatalf("before the void the later stamp wins, got %q", got)
	}
	void := proj.Op{Ref: proj.Ref{Origin: "cloud-1", Stream: stream, Seq: 1}, HLC: hlc(2000, 0), Intent: proj.IntentVoid,
		Muts: []proj.Mut{{E: proj.EntityVoid, ID: stream + "|d2|1", K: proj.KInsert, F: map[string]any{"origin": "d2", "stream": stream, "seq": int64(1), "reason": "refused"}}}}
	apply(void)
	if got := itemNames(t, svc, As(t, tenant))[item1]; got != "Legit edit" {
		t.Fatalf("a void must restore the legitimate earlier edit, got %q", got)
	}
	apply(void) // replayed void: no-op
	// A void of the only insert removes the row.
	apply(proj.Op{Ref: proj.Ref{Origin: "cloud-1", Stream: stream, Seq: 2}, HLC: hlc(2001, 0), Intent: proj.IntentVoid,
		Muts: []proj.Mut{{E: proj.EntityVoid, ID: "x", K: proj.KInsert, F: map[string]any{"origin": "d1", "stream": stream, "seq": int64(1)}}}})
	if got := itemNames(t, svc, As(t, tenant)); len(got) != 0 {
		t.Fatalf("voiding the insert must remove the row, have %v", got)
	}
}

func voidFirst(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	void := proj.Op{Ref: proj.Ref{Origin: "cloud-1", Stream: stream, Seq: 1}, HLC: hlc(2000, 0), Intent: proj.IntentVoid,
		Muts: []proj.Mut{{E: proj.EntityVoid, ID: "x", K: proj.KInsert, F: map[string]any{"origin": "d1", "stream": stream, "seq": int64(1)}}}}
	for _, o := range []proj.Op{void, op("d1", stream, 1, hlc(1000, 0), insItem(item1, "Tea"))} {
		if err := svc.ApplyOpTx(ctxBg, tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	if got := itemNames(t, svc, As(t, tenant)); len(got) != 0 {
		t.Fatalf("an op voided before it arrived must have no effect, have %v", got)
	}
}

func priceConflict(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	apply := func(o proj.Op) {
		t.Helper()
		if err := svc.ApplyOpTx(ctxBg, tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	price := func(p int64, prev string) proj.Mut {
		m := proj.Mut{E: "price_entry", ID: price1, K: proj.KSet, F: map[string]any{"price_paise": p}}
		if prev != "" {
			m.Prev = map[string]string{"price_paise": prev}
		}
		return m
	}
	apply(op("d1", stream, 1, hlc(1000, 0), proj.Mut{E: "price_entry", ID: price1, K: proj.KInsert, F: map[string]any{"item_id": item1, "price_paise": int64(24000)}}))
	seen := proj.FormatHLC(hlc(1000, 0))
	apply(op("d1", stream, 2, hlc(1010, 0), price(24500, seen)))
	ctx := As(t, tenant)
	exc := func() []service.ExceptionView {
		pg, err := svc.ListExceptions(ctx, service.ListInput{Limit: 50}, "")
		if err != nil {
			t.Fatal(err)
		}
		return pg.Items
	}
	if len(exc()) != 0 {
		t.Fatal("a sequential edit is not a conflict")
	}
	apply(op("d2", stream, 1, hlc(1020, 0), price(25000, seen))) // d2 never saw d1's edit
	got := exc()
	if len(got) != 1 || got[0].Kind != "price_conflict" || got[0].Status != "open" || got[0].OutletID != outletA || got[0].SubjectID != price1 {
		t.Fatalf("want one open price_conflict for outlet %s, got %+v", outletA, got)
	}
	if got[0].Params["kept_paise"] != int64(25000) || got[0].Params["lost_paise"] != int64(24500) {
		t.Fatalf("kept/lost wrong: %v", got[0].Params)
	}
	apply(op("d2", stream, 1, hlc(1020, 0), price(25000, seen))) // replay: still one finding
	if len(exc()) != 1 {
		t.Fatal("re-applying the op must not add a finding")
	}
	apply(op("m", stream, 1, hlc(1030, 0), price(25000, proj.FormatHLC(hlc(1020, 0))))) // the manager saw the conflict
	if g := exc(); len(g) != 1 || g[0].Status != "superseded" {
		t.Fatalf("a later edit that saw the winner supersedes the finding: %+v", g)
	}
	open, err := svc.ListExceptions(ctx, service.ListInput{Limit: 50}, "open")
	if err != nil || len(open.Items) != 0 {
		t.Fatalf("status filter: %v %v", open.Items, err)
	}
}

func ignored(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	ctx := As(t, tenant)
	apply := func(o proj.Op) {
		t.Helper()
		if err := svc.ApplyOpTx(ctxBg, tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	apply(op("d1", stream, 1, hlc(1000, 0),
		proj.Mut{E: "bill", ID: "b1", K: proj.KInsert, F: map[string]any{"total_paise": int64(1)}}, // another owner's entity
		proj.Mut{E: "station", ID: "s1", K: proj.KInsert, F: map[string]any{"code": "K"}},          // outlet entity on a brand stream
		proj.Mut{E: "item", ID: item1, K: "rank", F: map[string]any{"name_en": "x"}},               // kind catalog does not use
		insItem(item2, "Kept")))
	apply(op("d1", "tenant:"+tenant.String(), 1, hlc(1001, 0), insItem("other", "Tenant stream")))
	apply(op("d1", "brand:not-a-uuid", 1, hlc(1002, 0), insItem("bad", "Bad stream")))
	if got := itemNames(t, svc, ctx); len(got) != 1 || got[item2] != "Kept" {
		t.Fatalf("only the catalog-owned, well-formed mutation applies: %v", got)
	}
	st, err := svc.ListStations(ctx, service.ListInput{Limit: 10})
	if err != nil || len(st.Items) != 0 {
		t.Fatalf("an outlet entity on a brand stream must be ignored: %v %v", st.Items, err)
	}
	if err := svc.ApplyEventTx(ctxBg, events.Envelope{ID: ids.New(), Type: "billing.bill.recorded", V: 1, Source: "billing", TenantID: tenant, Key: "k", OccurredAt: seed.At, Data: json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatalf("an event catalog does not consume is ignored: %v", err)
	}
}

type fetcher struct {
	body string
	err  error
}

func (f fetcher) Body(context.Context, ids.ID, string, string, int64) (string, error) {
	return f.body, f.err
}

func opEvent(tenant ids.ID, typ string, data map[string]any) events.Envelope {
	b, _ := json.Marshal(data)
	return events.Envelope{ID: ids.New(), Type: typ, V: 1, Source: "sync", TenantID: tenant, Key: "k", OccurredAt: seed.At, Data: b}
}

func opEvents(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := seed.Example.Org()
	setup(t, svc, tenant)
	stream := "brand:" + brandA.String()
	body := `{"origin":"dev-1","stream":"` + stream + `","seq":1,"hlc":"` + proj.FormatHLC(hlc(1000, 0)) + `","intent":"menu.item.edit","muts":[{"e":"item","id":"` + item1 + `","k":"insert","f":{"category_id":"` + catID + `","name_en":"Via event","kitchen_name":"Via event","veg_mark":"none","tax_class":"fnb_standard","status":"active","sort_order":1}}]}`
	base := map[string]any{"origin": "dev-1", "stream": stream, "seq": 1, "intent": "menu.item.edit", "status": "accepted"}
	ev := func(extra map[string]any) events.Envelope {
		d := map[string]any{}
		for k, v := range base {
			d[k] = v
		}
		for k, v := range extra {
			d[k] = v
		}
		return opEvent(tenant, "sync.op.stored", d)
	}
	if err := svc.ApplyEventTx(ctxBg, ev(map[string]any{"status": "quarantined", "body": body}), nil); err != nil {
		t.Fatal(err)
	}
	if got := itemNames(t, svc, As(t, tenant)); len(got) != 0 {
		t.Fatal("a quarantined op must not be applied")
	}
	if err := svc.ApplyEventTx(ctxBg, ev(nil), nil); !errors.Is(err, service.ErrOpBodyUnavailable) {
		t.Fatalf("an event without a body and no fetcher is retried then parked: %v", err)
	}
	if err := svc.ApplyEventTx(ctxBg, ev(map[string]any{"seq": 2, "body": body}), nil); err == nil {
		t.Fatal("a body whose identity differs from its event must be refused")
	}
	if err := svc.ApplyEventTx(ctxBg, ev(nil), fetcher{body: body}); err != nil {
		t.Fatalf("body fetched from the gateway: %v", err)
	}
	if err := svc.ApplyEventTx(ctxBg, ev(map[string]any{"body": body, "status": "flagged"}), nil); err != nil {
		t.Fatalf("a flagged op is applied (and a replay is a no-op): %v", err)
	}
	if got := itemNames(t, svc, As(t, tenant)); got[item1] != "Via event" {
		t.Fatalf("op applied from its event: %v", got)
	}
}

func tenants(t *testing.T, mk Factory) {
	svc := mk(t)
	a, b := seed.Example, seed.Other
	for _, w := range []seed.World{a, b} {
		if err := w.Apply(ctxBg, svc, seed.Base); err != nil {
			t.Fatal(err)
		}
	}
	ga, gb := itemNames(t, svc, As(t, a.Org())), itemNames(t, svc, As(t, b.Org()))
	if len(ga) != 1 || len(gb) != 1 || ga[a.Item().String()] == "" || gb[b.Item().String()] == "" {
		t.Fatalf("each tenant sees only its own item: %v %v", ga, gb)
	}
	if _, err := svc.GetItem(As(t, b.Org()), a.Item(), ""); err == nil {
		t.Fatal("tenant B read tenant A's item")
	}
}
