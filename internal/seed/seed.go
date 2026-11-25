// Package seed builds the example worlds of the catalog OpenAPI document (info.x-example-worlds): base,
// menu-empty, import-previewed and price-conflict. The contract suite sends requests built from the examples, so the double
// and a real service must hold exactly the rows the examples describe. Rows are loaded the way production loads
// them: as ops through the projection, so seeding also exercises the apply path. Other is a second, unrelated
// organisation for isolation tests.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
	"github.com/hostryhq/svc-catalog/service"
)

// At is the instant of the examples.
var At = time.Date(2026, 11, 24, 13, 5, 11, 120_000_000, time.UTC)

// Names of the example worlds.
const (
	Base            = "base"
	MenuEmpty       = "menu-empty"
	ImportPreviewed = "import-previewed"
	PriceConflict   = "price-conflict"
	// BaseWithImport is not a world of the spec: the base menu plus a previewed job, for isolation and guard tests
	// that need both.
	BaseWithImport = "base-with-import"
)

// World is one organisation's fixture. Ids are Prefix plus a four-digit suffix, so two worlds never collide.
type World struct {
	Prefix string
	// TwoOutlets adds a second outlet of the brand with its own set-up rows (outlet-scope tests).
	TwoOutlets bool
	// SecondBrand puts that second outlet under a second brand (brand selection of the menu import).
	SecondBrand bool
}

// Example is the organisation of the spec examples; Other is a second organisation.
var (
	Example = World{Prefix: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f"}
	Other   = World{Prefix: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3e", TwoOutlets: true}
	// TwoBrands is an organisation with two outlets of two brands.
	TwoBrands = World{Prefix: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3d", TwoOutlets: true, SecondBrand: true}
)

func (w World) id(s string) ids.ID { return ids.ID(w.Prefix + s) }

// Ids of the world (the suffixes are those of the spec examples).
func (w World) Org() ids.ID       { return w.id("4a60") }
func (w World) Outlet() ids.ID    { return w.id("4a61") }
func (w World) OutletY() ids.ID   { return w.id("4a71") }
func (w World) Brand() ids.ID     { return w.id("4a6a") }
func (w World) Brand2() ids.ID    { return w.id("4a7a") } // the brand of the second outlet when SecondBrand is set
func (w World) Category() ids.ID  { return w.id("5001") }
func (w World) Item() ids.ID      { return w.id("5101") }
func (w World) Variant() ids.ID   { return w.id("5111") }
func (w World) Group() ids.ID     { return w.id("5121") }
func (w World) Modifier() ids.ID  { return w.id("5122") }
func (w World) Station() ids.ID   { return w.id("5131") }
func (w World) Printer() ids.ID   { return w.id("5141") }
func (w World) Floor() ids.ID     { return w.id("5151") }
func (w World) Table() ids.ID     { return w.id("5152") }
func (w World) Reason() ids.ID    { return w.id("5161") }
func (w World) Import() ids.ID    { return w.id("5171") }
func (w World) Exception() ids.ID { return w.id("5181") }

// PriceRow is the deterministic id of the price of item 5101, variant 5111 (entity price_entry, UUIDv5).
func (w World) PriceRow() ids.ID {
	return ids.ID(service.PriceEntryID(w.Item().String(), w.Variant().String()))
}

// IDs is the id source of a service under the example worlds: ids queued by Next come out first (a creating
// operation of an example world mints the id its response example shows), then the generator's.
type IDs struct {
	mu   sync.Mutex
	gen  *ids.Generator
	next []ids.ID
}

// NewIDs wraps gen.
func NewIDs(gen *ids.Generator) *IDs { return &IDs{gen: gen} }

// New implements service.IDSource.
func (s *IDs) New() ids.ID {
	s.mu.Lock()
	if len(s.next) > 0 {
		id := s.next[0]
		s.next = s.next[1:]
		s.mu.Unlock()
		return id
	}
	s.mu.Unlock()
	return s.gen.New()
}

// Next replaces the queue with the ids the world's creating operation must mint: in menu-empty the import job
// of createMenuImport is 5171.
func (s *IDs) Next(w World, world string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = nil
	if world == MenuEmpty {
		s.next = []ids.ID{w.Import()}
	}
}

// ClockAdvance is how far the example clock stands after the world is loaded: the commit example of the
// import-previewed world happens four minutes after the preview (committed_at 13:09:11.120Z).
func ClockAdvance(world string) time.Duration {
	if world == ImportPreviewed {
		return 4 * time.Minute
	}
	return 0
}

// StationY, FloorY and TableY are the second outlet's rows.
func (w World) StationY() ids.ID { return w.id("5231") }
func (w World) FloorY() ids.ID   { return w.id("5251") }

type seq struct {
	n    int64
	hlc  uint64
	last string
}

func (s *seq) op(tenant ids.ID, stream string, muts ...gsync.Mut) *gsync.Op {
	s.n++
	s.hlc += 1 << 16
	return &gsync.Op{Ref: gsync.OpRef{Origin: "seed-device", Stream: stream, Seq: s.n}, HLC: fmt.Sprintf("%016x", s.hlc), Intent: "menu.item.edit", IntentV: 1,
		TenantID: tenant, Muts: muts}
}

func ins(e string, id ids.ID, f map[string]any) gsync.Mut {
	return gsync.Mut{E: e, ID: id.String(), K: gsync.KindInsert, F: f}
}

// Apply loads the named world for w into svc.
func (w World) Apply(ctx context.Context, svc *service.Service, world string) error {
	tenant := w.Org()
	outlets := []ids.ID{w.Outlet()}
	if w.TwoOutlets {
		outlets = append(outlets, w.OutletY())
	}
	for _, o := range outlets {
		brand := w.Brand()
		if w.SecondBrand && o == w.OutletY() {
			brand = w.Brand2()
		}
		data, _ := json.Marshal(map[string]any{"outlet_id": o, "brand_id": brand, "kind": "restaurant", "status": "active", "tax_registration_id": w.id("4a6b")})
		env := events.Envelope{ID: ids.New(), Type: service.EventOutletCreated, V: 1, Source: "tenancy", TenantID: tenant, OutletID: o, Key: o.String(), OccurredAt: At, Data: data}
		if err := svc.ApplyEventTx(ctx, env); err != nil {
			return err
		}
	}
	var sq seq
	brand := "brand:" + w.Brand().String()
	menu := world == Base || world == PriceConflict || world == BaseWithImport // menu-empty and import-previewed hold no menu rows
	var ops []*gsync.Op
	if menu {
		ops = append(ops, sq.op(tenant, brand,
			ins("category", w.Category(), map[string]any{"name_en": "Starters", "default_station_code": "KITCHEN", "default_tax_class": "fnb_standard", "sort_order": int64(10), "status": "active"}),
			ins("item", w.Item(), map[string]any{"category_id": w.Category().String(), "name_en": "Paneer Tikka", "kitchen_name": "Paneer Tikka", "station_code": "KITCHEN",
				"veg_mark": "veg", "tax_class": "fnb_standard", "code": "101", "status": "active", "sort_order": int64(1)}),
			ins("item_variant", w.Variant(), map[string]any{"item_id": w.Item().String(), "name": "Full", "is_default": true, "sort_order": int64(1)}),
			ins("price_entry", w.PriceRow(), map[string]any{"item_id": w.Item().String(), "variant_id": w.Variant().String(), "price_paise": int64(24000)}),
			ins("modifier_group", w.Group(), map[string]any{"name": "Add-ons", "min_select": int64(0), "max_select": int64(2)}),
			ins("modifier", w.Modifier(), map[string]any{"group_id": w.Group().String(), "name": "Extra cheese", "price_delta_paise": int64(2000), "sort_order": int64(1)}),
			ins("item_modifier_group", ids.ID(gsync.UUIDv5("item_modifier_group|"+w.Item().String()+"|"+w.Group().String())), map[string]any{"item_id": w.Item().String(), "group_id": w.Group().String(), "sort_order": int64(1)}),
			ins("reason_code", w.Reason(), map[string]any{"purpose": "cancel", "label_key": "reason.cancel.guest_changed_mind", "active": true})))
	}
	for _, o := range outlets {
		out := "outlet:" + o.String()
		st, fl, tb, pr := w.Station(), w.Floor(), w.Table(), w.Printer()
		if o == w.OutletY() {
			st, fl, tb, pr = w.StationY(), w.FloorY(), w.id("5252"), w.id("5241")
		}
		muts := []gsync.Mut{
			ins("printer", pr, map[string]any{"name": "Kitchen LAN", "connection": "lan", "address": "192.0.2.50", "port": int64(9100), "paper_width_mm": int64(80),
				"chars_per_line": int64(48), "code_page": "cp437", "cut": true, "drawer_pin": int64(0), "raster": true, "power_backup": "none"}),
			ins("station", st, map[string]any{"code": "KITCHEN", "name": "Kitchen", "output": "both", "printer_id": pr.String(), "language": "ta"}),
			ins("floor", fl, map[string]any{"name": "Ground floor", "sort_order": int64(1)}),
			ins("dining_table", tb, map[string]any{"floor_id": fl.String(), "name": "T1", "seats": int64(4), "active": true})}
		if menu {
			muts = append(muts, ins("item_outlet", ids.ID(gsync.UUIDv5("item_outlet|"+o.String()+"|"+w.Item().String())), map[string]any{"item_id": w.Item().String(), "available": true, "quick_item": true}))
		}
		ops = append(ops, sq.op(tenant, out, muts...))
	}
	for _, op := range ops {
		if err := svc.ApplyOpTx(ctx, op); err != nil {
			return err
		}
	}
	switch world {
	case ImportPreviewed, BaseWithImport:
		return w.insertImport(ctx, svc)
	case PriceConflict:
		return w.insertException(ctx, svc)
	}
	return nil
}

// insertImport stores the preview of the example CSV: it creates category 5001 Starters and item 5101 Paneer
// Tikka (code 101) with variant 5111 Full at 24000, the rows the menu-empty world lacks.
func (w World) insertImport(ctx context.Context, svc *service.Service) error {
	plan := &model.ImportPlan{BrandID: w.Brand().String(),
		Categories: []model.PlanCategory{{ID: w.Category().String(), NameEn: "Starters", Station: "KITCHEN", TaxClass: "fnb_standard", Sort: 10}},
		Items: []model.PlanItem{{ID: w.Item().String(), Code: "101", NameEn: "Paneer Tikka", KitchenEn: "Paneer Tikka", CategoryID: w.Category().String(), Station: "KITCHEN", VegMark: "veg",
			TaxClass: "fnb_standard", Sort: 1, Variants: []model.PlanVariant{{ID: w.Variant().String(), Name: "Full", PricePaise: 24000, PriceID: w.PriceRow().String(), Default: true, Sort: 1}}}}}
	return inTenant(ctx, svc, w.Org(), func(tx repository.Tx) error {
		return tx.InsertImport(ctx, model.Import{ID: w.Import(), Status: model.ImportPreviewed, Source: model.SourceOwner, Filename: "menu.csv", RowCount: 1, Errors: []model.ImportRowError{},
			CategoriesCreated: 1, ItemsCreated: 1, CreatedAt: At, Plan: plan})
	})
}

func (w World) insertException(ctx context.Context, svc *service.Service) error {
	return inTenant(ctx, svc, w.Org(), func(tx repository.Tx) error {
		return tx.UpsertException(ctx, model.Exception{ID: w.Exception(), Kind: model.KindPriceConflict, Severity: "warning", Status: model.StatusOpen, SubjectKind: "price_entry",
			SubjectID: w.PriceRow().String(), OutletID: w.Outlet(), DetectedAt: At, DetailKey: "exception.price_conflict", DedupeKey: "seed|price_conflict",
			Params: map[string]any{"kept_paise": int64(25000), "lost_paise": int64(24000)}})
	})
}

func inTenant(ctx context.Context, svc *service.Service, tenant ids.ID, fn func(tx repository.Tx) error) error {
	ctx = tenancy.With(ctx, tenancy.Context{TenantID: tenant})
	return svc.Repo().InTx(ctx, func(_ context.Context, tx repository.Tx) error { return fn(tx) })
}
