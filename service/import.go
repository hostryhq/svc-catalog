package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"

	cev "github.com/hostryhq/svc-catalog/events"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
)

// PermSupportImport is the permission a support act-mode session exercises to import (it widens menu.import).
const PermSupportImport = "catalog.import"

// IntentImport is the intent of the emitted import ops (api-specs entities: item and price_entry list
// menu.import.apply among their written_by_intents).
const IntentImport = "menu.import.apply"

func importSource(ctx context.Context) string {
	if p, ok := auth.PrincipalFrom(ctx); ok && p.Type == auth.TypeSupport {
		return model.SourceSupport
	}
	return model.SourceOwner
}

// CreateMenuImport validates the CSV, stores the preview job and returns it. Nothing is emitted until commit.
// An import of a file with errors is stored too (the owner reads its row errors) but cannot be committed.
//
// The menu belongs to a brand: brand names it, else outlet (X-Outlet-Id) does, else the organisation's only
// brand. A support act-mode session may only add (new categories, items, variants): a row that would change an
// existing item, variant or price is refused 422 owner_commit_required, and the owner imports it themselves.
func (s *Service) CreateMenuImport(ctx context.Context, filename, csvText string, brand ids.ID, outlet string) (model.Import, error) {
	return s.createMenuImport(ctx, importSource(ctx), filename, csvText, brand, outlet)
}

func (s *Service) createMenuImport(ctx context.Context, source, filename, csvText string, brand ids.ID, outlet string) (model.Import, error) {
	if strings.TrimSpace(filename) == "" {
		return model.Import{}, model.Invalid("filename", ErrRequired, "a file name is required")
	}
	rows, err := parseCSV(csvText)
	if err != nil {
		return model.Import{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Import, error) {
		job := s.newID() // minted first: the id of a created job is the one an example world expects
		sc, _, err := s.scope(ctx, tx, PermImport, outlet)
		if err != nil {
			return model.Import{}, err
		}
		brand, err := s.importBrand(ctx, tx, sc, brand)
		if err != nil {
			return model.Import{}, err
		}
		plan, errs, counts, err := s.buildPlan(ctx, tx, brand, rows)
		if err != nil {
			return model.Import{}, err
		}
		if source == model.SourceSupport && len(errs) == 0 && changesExisting(plan) {
			return model.Import{}, apperr.New(apperr.Validation, "owner_commit_required",
				"a support import may only add; rows that change an existing item, variant or price need the owner's own import")
		}
		im := model.Import{ID: job, Status: model.ImportPreviewed, Source: source, Filename: truncate(filename, 100), RowCount: len(rows),
			ErrorCount: len(errs), Errors: errs, CategoriesCreated: counts.categories, ItemsCreated: counts.created, ItemsUpdated: counts.updated,
			PriceChanges: counts.priceChanges, CreatedAt: now(s), CreatedBy: actor(ctx), Plan: plan}
		if len(im.Errors) > maxReportedErrors {
			im.Errors = im.Errors[:maxReportedErrors]
		}
		if len(errs) > 0 {
			im.Plan = nil // nothing to emit: the preview cannot be committed
		}
		if err := tx.InsertImport(ctx, im); err != nil {
			return model.Import{}, err
		}
		return im, tx.AppendAudit(ctx, model.AuditEntry{ID: s.newID(), At: im.CreatedAt, Actor: actor(ctx), Action: PermImport + ".preview", SubjectKind: "menu_import", SubjectID: im.ID.String(), After: im.Source})
	})
}

// importBrand picks the brand an import targets: the one named (404 when it is not a brand of the caller's
// outlets, so another organisation's brand is indistinguishable from an unknown one), else the brand of the
// outlet in scope when there is exactly one, else 422 brand_required.
func (s *Service) importBrand(ctx context.Context, tx repository.Tx, sc auth.OutletScope, want ids.ID) (ids.ID, error) {
	b, err := tx.BrandsOf(ctx, sc)
	if err != nil {
		return "", err
	}
	if want != "" {
		if !slices.Contains(b, want) {
			return "", model.ErrNotFound
		}
		return want, nil
	}
	switch len(b) {
	case 0:
		return "", model.Invalid("brand_id", "no_outlet", "the organisation has no outlet yet")
	case 1:
		return b[0], nil
	}
	return "", apperr.New(apperr.Validation, "brand_required", "the organisation has several brands; name the brand or an outlet")
}

// changesExisting reports whether the plan changes anything that already exists: a renamed item or a changed price.
// Adding categories, items and variants is not a change.
func changesExisting(plan *model.ImportPlan) bool {
	for _, it := range plan.Items {
		if it.NameChanged {
			return true
		}
		for _, v := range it.Variants {
			if v.PriceChanged {
				return true
			}
		}
	}
	return false
}

type planCounts struct{ categories, created, updated, priceChanges int }

type group struct {
	first    csvRow
	code     string
	variants []csvRow
}

func (s *Service) defaultTaxClass() string {
	for _, c := range s.taxClasses {
		if c == "fnb_standard" {
			return c
		}
	}
	return s.taxClasses[0]
}

func (s *Service) buildPlan(ctx context.Context, tx repository.Tx, brand ids.ID, rows []csvRow) (*model.ImportPlan, []model.ImportRowError, planCounts, error) {
	var errs []model.ImportRowError
	var counts planCounts
	var valid []csvRow
	for _, r := range rows {
		if e := rowErrors(r); len(e) > 0 {
			errs = append(errs, e...)
			continue
		}
		valid = append(valid, r)
	}
	// Group rows into items: by item code, else by category and name. Rows of one item are its variants.
	var order []string
	groups := map[string]*group{}
	for _, r := range valid {
		key := "name:" + lower(r.category) + "|" + lower(r.name)
		if r.code != "" {
			key = "code:" + r.code
		}
		g := groups[key]
		if g == nil {
			g = &group{first: r, code: r.code}
			groups[key] = g
			order = append(order, key)
		} else {
			veg1, _ := normVeg(g.first.veg)
			veg2, _ := normVeg(r.veg)
			for _, c := range []struct {
				field string
				diff  bool
			}{
				{"category", lower(g.first.category) != lower(r.category)}, {"item name", lower(g.first.name) != lower(r.name)},
				{"veg mark", veg1 != veg2}, {"station", strings.ToUpper(g.first.station) != strings.ToUpper(r.station)},
			} {
				if c.diff {
					errs = append(errs, model.ImportRowError{Row: r.n, Field: c.field, Code: ErrItemConflict})
				}
			}
		}
		vn := variantName(r)
		dup := false
		for _, v := range g.variants {
			dup = dup || lower(variantName(v)) == lower(vn)
		}
		if dup {
			errs = append(errs, model.ImportRowError{Row: r.n, Field: "variant name", Code: ErrVariantDup})
			continue
		}
		g.variants = append(g.variants, r)
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Row < errs[j].Row })
	plan := &model.ImportPlan{BrandID: brand.String()}
	if len(errs) > 0 {
		return plan, errs, counts, nil
	}

	brands := repository.BrandSet{IDs: []ids.ID{brand}}
	cats, err := tx.ListRows(ctx, repository.RowQuery{Entity: "category", Brands: brands})
	if err != nil {
		return nil, nil, counts, err
	}
	catByName := map[string]model.Row{}
	for _, c := range cats {
		catByName[lower(c.Str("name_en"))] = c
	}
	var codes []string
	for _, k := range order {
		if c := groups[k].code; c != "" {
			codes = append(codes, c)
		}
	}
	existing := map[string]model.Row{} // by item code
	variants := map[string][]model.Row{}
	prices := map[string]model.Row{} // item|variant -> existing price row
	if len(codes) > 0 {
		items, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item", Brands: brands, In: map[string][]string{"code": codes}})
		if err != nil {
			return nil, nil, counts, err
		}
		var itemIDs []string
		for _, it := range items {
			existing[it.Str("code")] = it
			itemIDs = append(itemIDs, it.ID)
		}
		if len(itemIDs) > 0 {
			in := map[string][]string{"item_id": itemIDs}
			vs, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item_variant", Brands: brands, In: in})
			if err != nil {
				return nil, nil, counts, err
			}
			for _, v := range vs {
				variants[v.Str("item_id")] = append(variants[v.Str("item_id")], v)
			}
			ps, err := tx.ListRows(ctx, repository.RowQuery{Entity: "price_entry", Brands: brands, In: in})
			if err != nil {
				return nil, nil, counts, err
			}
			for _, p := range ps {
				prices[p.Str("item_id")+"|"+p.Str("variant_id")] = p
			}
		}
	}

	catPlan := map[string]*model.PlanCategory{}
	for _, k := range order {
		g := groups[k]
		cn := lower(g.first.category)
		pc := catPlan[cn]
		if pc == nil {
			pc = &model.PlanCategory{NameEn: g.first.category, Station: strings.ToUpper(g.first.station), TaxClass: s.defaultTaxClass(), Sort: (len(cats) + len(catPlan) + 1) * 10}
			if ex, ok := catByName[cn]; ok {
				pc.ID, pc.Exists, pc.TaxClass = ex.ID, true, orDefault(ex.Str("default_tax_class"), s.defaultTaxClass())
			} else {
				pc.ID = s.newID().String()
				plan.Categories = append(plan.Categories, *pc)
				counts.categories++
			}
			catPlan[cn] = pc
		}
		veg, _ := normVeg(g.first.veg)
		it := model.PlanItem{Code: g.code, NameEn: g.first.name, KitchenEn: truncate(g.first.name, 20), CategoryID: pc.ID, Station: strings.ToUpper(g.first.station),
			VegMark: veg, TaxClass: pc.TaxClass, Sort: len(plan.Items) + 1}
		ex, matched := existing[g.code]
		if g.code == "" {
			matched = false
		}
		if matched {
			it.ID, it.Exists, it.NameChanged = ex.ID, true, ex.Str("name_en") != g.first.name
			counts.updated++
			// An existing item keeps its category, kitchen name, station, veg mark and tax class: a re-import
			// updates names and prices (RM-M9).
		} else {
			it.ID = s.newID().String()
			counts.created++
		}
		for i, v := range g.variants {
			paise, _ := priceToPaise(v.price)
			pv := model.PlanVariant{Name: variantName(v), PricePaise: paise, Sort: i + 1, Default: i == 0 && !matched}
			if matched {
				for _, ev := range variants[it.ID] {
					if lower(ev.Str("name")) == lower(pv.Name) {
						pv.ID, pv.Exists = ev.ID, true
						if cur, ok := prices[it.ID+"|"+ev.ID]; ok {
							pv.PriceID = cur.ID // the existing row, whatever its id
							if cur.Int("price_paise") != paise {
								counts.priceChanges++
								pv.PriceChanged = true
							}
						}
					}
				}
			}
			if pv.ID == "" {
				pv.ID = s.newID().String()
			}
			if pv.PriceID == "" {
				pv.PriceID = PriceEntryID(it.ID, pv.ID)
			}
			it.Variants = append(it.Variants, pv)
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil, counts, nil
}

func variantName(r csvRow) string {
	if r.variant != "" {
		return r.variant
	}
	return truncate(r.name, 40)
}

// GetMenuImport returns one job.
func (s *Service) GetMenuImport(ctx context.Context, id ids.ID) (model.Import, error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Import, error) {
		if _, _, err := s.scope(ctx, tx, PermImport, ""); err != nil {
			return model.Import{}, err
		}
		im, err := tx.GetImport(ctx, id)
		return im, repoErr(err)
	})
}

// ListMenuImports lists jobs, newest first.
func (s *Service) ListMenuImports(ctx context.Context, after string, limit int) (Page[model.Import], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Import], error) {
		if _, _, err := s.scope(ctx, tx, PermImport, ""); err != nil {
			return Page[model.Import]{}, err
		}
		rows, err := tx.ListImports(ctx, ids.ID(after), limit+1)
		if err != nil {
			return Page[model.Import]{}, err
		}
		return pageOf(rows, limit, func(im model.Import) string { return im.ID.String() }), nil
	})
}

// CancelMenuImport abandons a previewed import. Cancelling a cancelled job is a no-op (a retry);
// a committed job cannot be cancelled.
func (s *Service) CancelMenuImport(ctx context.Context, id ids.ID) (model.Import, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Import, error) {
		if _, _, err := s.scope(ctx, tx, PermImport, ""); err != nil {
			return model.Import{}, err
		}
		im, err := tx.GetImport(ctx, id)
		if err != nil {
			return model.Import{}, repoErr(err)
		}
		switch im.Status {
		case model.ImportCancelled:
			return im, nil
		case model.ImportCommitted:
			return model.Import{}, errNotPreviewed()
		}
		im.Status, im.Plan = model.ImportCancelled, nil
		if err := tx.UpdateImport(ctx, im); err != nil {
			return model.Import{}, err
		}
		return im, tx.AppendAudit(ctx, model.AuditEntry{ID: s.newID(), At: now(s), Actor: actor(ctx), Action: PermImport + ".cancel", SubjectKind: "menu_import", SubjectID: im.ID.String()})
	})
}

// CommitMenuImport emits the previewed import as ops: all rows or none. It is refused while the preview has
// errors (422 import_has_errors) and once the job is committed or cancelled (409 import_not_previewed). The
// ops carry keys derived from the job id, so a retry cannot duplicate them at the gateway.
func (s *Service) CommitMenuImport(ctx context.Context, id ids.ID, rowCount int) (model.Import, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return model.Import{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Import, error) {
		if _, _, err := s.scope(ctx, tx, PermImport, ""); err != nil {
			return model.Import{}, err
		}
		im, err := tx.GetImport(ctx, id)
		if err != nil {
			return model.Import{}, repoErr(err)
		}
		if im.Status != model.ImportPreviewed {
			return model.Import{}, errNotPreviewed()
		}
		if im.ErrorCount > 0 || im.Plan == nil {
			return model.Import{}, apperr.New(apperr.Validation, "import_has_errors", "the preview has row errors; fix the file and import again")
		}
		if rowCount != im.RowCount {
			return model.Import{}, model.Invalid("row_count", "row_count_mismatch", "row_count does not match the preview")
		}
		brands := repository.BrandSet{IDs: []ids.ID{ids.ID(im.Plan.BrandID)}}
		prev, err := s.currentPriceStamps(ctx, tx, brands, im.Plan)
		if err != nil {
			return model.Import{}, err
		}
		// Queued emission (ADR-0014 A2.2 item 7): the ops join emit_queue in this transaction, so they exist
		// exactly when the job is committed, and a worker sends them in order whatever the gateway does.
		perm := PermImport
		if im.Source == model.SourceSupport {
			perm = PermSupportImport
		}
		if err := tx.EmitEnqueue(ctx, importOps(id, im.Plan, s.importBatch, prev, emitActor(ctx), perm)...); err != nil {
			return model.Import{}, err
		}
		t := now(s)
		im.Status, im.CommittedAt, im.Plan = model.ImportCommitted, &t, nil
		if err := tx.UpdateImport(ctx, im); err != nil {
			return model.Import{}, err
		}
		if err := tx.AppendAudit(ctx, model.AuditEntry{ID: s.newID(), At: t, Actor: actor(ctx), Action: PermImport + ".commit", SubjectKind: "menu_import",
			SubjectID: im.ID.String(), After: fmt.Sprintf("%s rows=%d items_created=%d items_updated=%d price_changes=%d", im.Source, im.RowCount, im.ItemsCreated, im.ItemsUpdated, im.PriceChanges)}); err != nil {
			return model.Import{}, err
		}
		ev := cev.MenuImportDone(s.newID(), t, actor(ctx), tenant, cev.MenuImportData{ImportID: im.ID, Source: im.Source, RowCount: im.RowCount,
			ItemsCreated: im.ItemsCreated, ItemsUpdated: im.ItemsUpdated, PriceChanges: im.PriceChanges})
		return im, s.publish(ctx, tx, ev)
	})
}

// currentPriceStamps reads the stamp of each existing price row an import will edit, so the edit carries the
// `prev` of a writer who saw it (RM-M4: an import is a price edit).
func (s *Service) currentPriceStamps(ctx context.Context, tx repository.Tx, brands repository.BrandSet, plan *model.ImportPlan) (map[string]model.Row, error) {
	var itemIDs []string
	for _, it := range plan.Items {
		if it.Exists {
			itemIDs = append(itemIDs, it.ID)
		}
	}
	out := map[string]model.Row{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	ps, err := tx.ListRows(ctx, repository.RowQuery{Entity: "price_entry", Brands: brands, In: map[string][]string{"item_id": itemIDs}})
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		out[p.ID] = p
	}
	return out, nil
}

// importOps turns a plan into ops: one for the new categories and one per batch of items. Keys are
// deterministic in the job id and the position, so a replayed commit is the same ops at the gateway.
func importOps(job ids.ID, plan *model.ImportPlan, batch int, prev map[string]model.Row, who, perm string) []emit.Op {
	stream := "brand:" + plan.BrandID
	op := func(key string, muts []gsync.Mut) emit.Op {
		return emit.Op{Key: emit.DeterministicKey("menu-import", job.String(), key), Stream: stream, Intent: IntentImport, IntentV: 1,
			Muts: muts, Actor: who, AuthorisedBy: &perm}
	}
	var out []emit.Op
	var cm []gsync.Mut
	for _, c := range plan.Categories {
		f := map[string]any{"name_en": c.NameEn, "default_tax_class": c.TaxClass, "sort_order": int64(c.Sort), "status": "active"}
		if c.Station != "" {
			f["default_station_code"] = c.Station
		}
		cm = append(cm, gsync.Mut{E: "category", ID: c.ID, K: gsync.KindInsert, F: f})
	}
	if len(cm) > 0 {
		out = append(out, op("c", cm))
	}
	for start, n := 0, 0; start < len(plan.Items); start, n = start+batch, n+1 {
		end := min(start+batch, len(plan.Items))
		var ms []gsync.Mut
		for _, it := range plan.Items[start:end] {
			ms = append(ms, itemMuts(it, prev)...)
		}
		out = append(out, op(fmt.Sprintf("i%d", n), ms))
	}
	return out
}

// PriceEntryID is the deterministic id of the price row of an item and variant (entity file price_entry:
// price_entry|{item_id}|{variant_id or "-"}), so two devices that price the same variant write one row.
func PriceEntryID(itemID, variantID string) string {
	if variantID == "" {
		variantID = "-"
	}
	return gsync.UUIDv5("price_entry|" + itemID + "|" + variantID)
}

func itemMuts(it model.PlanItem, prev map[string]model.Row) []gsync.Mut {
	var ms []gsync.Mut
	if it.Exists {
		if it.NameChanged {
			ms = append(ms, gsync.Mut{E: "item", ID: it.ID, K: gsync.KindSet, F: map[string]any{"name_en": it.NameEn}})
		}
	} else {
		f := map[string]any{"category_id": it.CategoryID, "name_en": it.NameEn, "kitchen_name": it.KitchenEn, "veg_mark": it.VegMark,
			"tax_class": it.TaxClass, "status": "active", "sort_order": int64(it.Sort)}
		if it.Code != "" {
			f["code"] = it.Code
		}
		if it.Station != "" {
			f["station_code"] = it.Station
		}
		ms = append(ms, gsync.Mut{E: "item", ID: it.ID, K: gsync.KindInsert, F: f})
	}
	for _, v := range it.Variants {
		if !v.Exists {
			ms = append(ms, gsync.Mut{E: "item_variant", ID: v.ID, K: gsync.KindInsert, F: map[string]any{"item_id": it.ID, "name": v.Name, "is_default": v.Default, "sort_order": int64(v.Sort)}})
		}
		if cur, ok := prev[v.PriceID]; ok {
			ms = append(ms, gsync.Mut{E: "price_entry", ID: v.PriceID, K: gsync.KindSet, F: map[string]any{"price_paise": v.PricePaise}, Prev: prevStamp(cur, "price_paise")})
		} else {
			ms = append(ms, gsync.Mut{E: "price_entry", ID: v.PriceID, K: gsync.KindInsert, F: map[string]any{"item_id": it.ID, "variant_id": v.ID, "price_paise": v.PricePaise}})
		}
	}
	return ms
}
