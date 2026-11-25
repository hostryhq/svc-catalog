package projsuite

import (
	"context"
	"testing"

	"github.com/hostryhq/go-auth/scopetest"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/repository"
	"github.com/hostryhq/svc-catalog/service"
)

// T14 runs go-auth's generated outlet-scope test (ADR-0003 A1.4, T14) for every outlet-scoped resource of the
// catalog: a member scoped to outlet X sees nothing of outlet Y in lists and reads, and an organisation-wide
// member sees both. A read of another outlet's row is the X-Outlet-Id 404.
func T14(t *testing.T, mk Factory) {
	svc := mk(t)
	tenant := testkit.TenantA
	brand := ids.ID("018f0000-0000-7000-8000-0000000000b1")
	for _, o := range []string{scopetest.OutletX, scopetest.OutletY} {
		if err := svc.ApplyEventTx(ctxBg, outletEvent(tenant, ids.ID(o), brand)); err != nil {
			t.Fatal(err)
		}
	}
	outletOf := map[string]string{}
	seq := int64(0)
	seedRows := func(entity string, f func(i string) map[string]any) func(ctx context.Context, x, y string) (string, string, error) {
		return func(ctx context.Context, x, y string) (string, string, error) {
			var rowIDs []string
			for _, o := range []string{x, y} {
				id := ids.New().String()
				seq++
				op := &gsync.Op{Ref: gsync.OpRef{Origin: "t14", Stream: "outlet:" + o, Seq: seq}, HLC: hs(uint64(1000+seq) << 16), Intent: "settings.outlet.edit", IntentV: 1,
					TenantID: tenant, Muts: []gsync.Mut{{E: entity, ID: id, K: gsync.KindInsert, F: f(id)}}}
				if err := svc.ApplyOpTx(ctxBg, op); err != nil {
					return "", "", err
				}
				outletOf[id] = o
				rowIDs = append(rowIDs, id)
			}
			return rowIDs[0], rowIDs[1], nil
		}
	}
	// Get reads through the list with the row's own outlet named, as a client does.
	get := func(list func(ctx context.Context, outlet string) ([]string, error)) func(ctx context.Context, id string) error {
		return func(ctx context.Context, id string) error {
			got, err := list(ctx, outletOf[id])
			if err != nil {
				return err
			}
			for _, g := range got {
				if g == id {
					return nil
				}
			}
			return model.ErrNotFound
		}
	}
	ids_ := func(n int, f func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	scoped := func(resource string, perm string, entity string, f func(i string) map[string]any, list func(ctx context.Context, outlet string) ([]string, error)) scopetest.Resource {
		return scopetest.Resource{Name: resource, Permission: perm, Seed: seedRows(entity, f),
			List:         func(ctx context.Context) ([]string, error) { return list(ctx, "") },
			ListFiltered: func(ctx context.Context, outlets []string) ([]string, error) { return list(ctx, outlets[0]) },
			Get:          get(list)}
	}
	in := func(outlet string) service.ListInput { return service.ListInput{Outlet: outlet, Limit: 200} }
	rowsOf := func(n int, id func(i int) string) []string { return ids_(n, id) }
	resources := []scopetest.Resource{
		scoped("stations", "settings.read", "station", func(string) map[string]any {
			return map[string]any{"code": "K", "name": "Kitchen", "output": "both", "language": "en"}
		}, func(ctx context.Context, o string) ([]string, error) {
			p, err := svc.ListStations(ctx, in(o))
			return rowsOf(len(p.Items), func(i int) string { return p.Items[i].ID }), err
		}),
		scoped("floors", "settings.read", "floor", func(string) map[string]any { return map[string]any{"name": "F", "sort_order": int64(1)} },
			func(ctx context.Context, o string) ([]string, error) {
				p, err := svc.ListFloors(ctx, in(o))
				return rowsOf(len(p.Items), func(i int) string { return p.Items[i].ID }), err
			}),
		scoped("dining tables", "settings.read", "dining_table", func(string) map[string]any {
			return map[string]any{"floor_id": "f", "name": "T1", "seats": int64(2), "active": true}
		}, func(ctx context.Context, o string) ([]string, error) {
			p, err := svc.ListDiningTables(ctx, in(o))
			return rowsOf(len(p.Items), func(i int) string { return p.Items[i].ID }), err
		}),
		scoped("printers", "settings.read", "printer", func(string) map[string]any {
			return map[string]any{"name": "P", "connection": "lan", "paper_width_mm": int64(80), "chars_per_line": int64(48), "code_page": "cp437", "cut": true,
				"drawer_pin": int64(0), "raster": true, "power_backup": "none"}
		}, func(ctx context.Context, o string) ([]string, error) {
			p, err := svc.ListPrinters(ctx, in(o))
			return rowsOf(len(p.Items), func(i int) string { return p.Items[i].ID }), err
		}),
		scoped("item overlay", "menu.read", "item_outlet", func(id string) map[string]any {
			return map[string]any{"item_id": id, "available": false, "quick_item": false}
		}, func(ctx context.Context, o string) ([]string, error) {
			p, err := svc.ListAvailability(ctx, in(o), nil)
			return rowsOf(len(p.Items), func(i int) string { return p.Items[i].RowID }), err
		}),
	}
	// Exceptions are rows of the repository, seeded directly: one finding per outlet.
	excOf := map[string]string{}
	resources = append(resources, scopetest.Resource{Name: "exceptions", Permission: "exceptions.view",
		Seed: func(ctx context.Context, x, y string) (string, string, error) {
			var out []string
			for _, o := range []string{x, y} {
				id := ids.New()
				err := svc.Repo().InTx(ctxFor(tenant), func(c context.Context, tx repository.Tx) error {
					return tx.UpsertException(c, model.Exception{ID: id, Kind: model.KindPriceConflict, Severity: "warning", Status: model.StatusOpen, SubjectKind: "price_entry",
						SubjectID: "s", OutletID: ids.ID(o), DetectedAt: ctxNow, DetailKey: "k", DedupeKey: id.String(), Params: map[string]any{}})
				})
				if err != nil {
					return "", "", err
				}
				excOf[id.String()], out = o, append(out, id.String())
			}
			return out[0], out[1], nil
		},
		List: func(ctx context.Context) ([]string, error) { return excList(svc, ctx, "") },
		ListFiltered: func(ctx context.Context, outlets []string) ([]string, error) {
			return excList(svc, ctx, outlets[0])
		},
		Get: func(ctx context.Context, id string) error {
			got, err := excList(svc, ctx, excOf[id])
			if err != nil {
				return err
			}
			for _, g := range got {
				if g == id {
					return nil
				}
			}
			return model.ErrNotFound
		}})
	scopetest.RunT14(t, catalogue(t), resources...)
}

func excList(svc *service.Service, ctx context.Context, outlet string) ([]string, error) {
	p, err := svc.ListExceptions(ctx, service.ListInput{Outlet: outlet, Limit: 200}, "")
	out := make([]string, len(p.Items))
	for i, e := range p.Items {
		out[i] = e.ID.String()
	}
	return out, err
}

var ctxNow = seed.At

func ctxFor(tenant ids.ID) context.Context {
	return tenancy.With(context.Background(), tenancy.Context{TenantID: tenant})
}
