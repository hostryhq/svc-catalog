package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	cev "github.com/hostryhq/svc-catalog/events"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/internal/uuid5"
	"github.com/hostryhq/svc-catalog/repository"
)

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
func (s *Service) CreateMenuImport(ctx context.Context, filename, csvText string) (model.Import, error) {
	if strings.TrimSpace(filename) == "" {
		return model.Import{}, model.Invalid("filename", ErrRequired, "a file name is required")
	}
	rows, err := parseCSV(csvText)
	if err != nil {
		return model.Import{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Import, error) {
		sc, _, err := s.scope(ctx, tx, PermImport, "")
		if err != nil {
			return model.Import{}, err
		}
		brand, err := s.importBrand(ctx, tx, sc)
		if err != nil {
			return model.Import{}, err
		}
		plan, errs, counts, err := s.buildPlan(ctx, tx, brand, rows)
		if err != nil {
			return model.Import{}, err
		}
		im := model.Import{ID: s.newID(), Status: model.ImportPreviewed, Source: importSource(ctx), Filename: truncate(filename, 100), RowCount: len(rows),
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

// importBrand picks the brand an import targets. The API names none (spec problem, docs/notes.md): Phase 2
// organisations have one brand, so the brand shared by the caller's outlets is used; several brands is a 422.
func (s *Service) importBrand(ctx context.Context, tx repository.Tx, sc auth.OutletScope) (ids.ID, error) {
	b, err := tx.BrandsOf(ctx, sc)
	if err != nil {
		return "", err
	}
	switch len(b) {
	case 0:
		return "", model.Invalid("brand", "no_outlet", "the organisation has no outlet yet")
	case 1:
		return b[0], nil
	}
	return "", model.Invalid("brand", "brand_ambiguous", "the organisation has several brands; an import targets one brand")
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
			it.ID, it.Exists = ex.ID, true
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
							}
						}
					}
				}
			}
			if pv.ID == "" {
				pv.ID = s.newID().String()
			}
			if pv.PriceID == "" {
				pv.PriceID = uuid5.PriceEntry(it.ID, pv.ID)
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
		for _, em := range importEmissions(id, im.Plan, s.importBatch, prev) {
			if _, err := s.emit(ctx, tenant, em); err != nil {
				return model.Import{}, err
			}
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

// importEmissions turns a plan into ops: one for the new categories and one per batch of items. Keys derive
// from the job id.
func importEmissions(job ids.ID, plan *model.ImportPlan, batch int, prev map[string]model.Row) []Emission {
	stream := "brand:" + plan.BrandID
	var out []Emission
	var cm []proj.Mut
	for _, c := range plan.Categories {
		f := map[string]any{"name_en": c.NameEn, "default_tax_class": c.TaxClass, "sort_order": int64(c.Sort), "status": "active"}
		if c.Station != "" {
			f["default_station_code"] = c.Station
		}
		cm = append(cm, proj.Mut{E: "category", ID: c.ID, K: proj.KInsert, F: f})
	}
	if len(cm) > 0 {
		out = append(out, Emission{Stream: stream, Intent: IntentImport, Key: "import|" + job.String() + "|c", Muts: cm})
	}
	for start, n := 0, 0; start < len(plan.Items); start, n = start+batch, n+1 {
		end := min(start+batch, len(plan.Items))
		var ms []proj.Mut
		for _, it := range plan.Items[start:end] {
			ms = append(ms, itemMuts(it, prev)...)
		}
		out = append(out, Emission{Stream: stream, Intent: IntentImport, Key: fmt.Sprintf("import|%s|i%d", job, n), Muts: ms})
	}
	return out
}

func itemMuts(it model.PlanItem, prev map[string]model.Row) []proj.Mut {
	var ms []proj.Mut
	if it.Exists {
		ms = append(ms, proj.Mut{E: "item", ID: it.ID, K: proj.KSet, F: map[string]any{"name_en": it.NameEn}})
	} else {
		f := map[string]any{"category_id": it.CategoryID, "name_en": it.NameEn, "kitchen_name": it.KitchenEn, "veg_mark": it.VegMark,
			"tax_class": it.TaxClass, "status": "active", "sort_order": int64(it.Sort)}
		if it.Code != "" {
			f["code"] = it.Code
		}
		if it.Station != "" {
			f["station_code"] = it.Station
		}
		ms = append(ms, proj.Mut{E: "item", ID: it.ID, K: proj.KInsert, F: f})
	}
	for _, v := range it.Variants {
		if !v.Exists {
			ms = append(ms, proj.Mut{E: "item_variant", ID: v.ID, K: proj.KInsert, F: map[string]any{"item_id": it.ID, "name": v.Name, "is_default": v.Default, "sort_order": int64(v.Sort)}})
		}
		if cur, ok := prev[v.PriceID]; ok {
			ms = append(ms, proj.Mut{E: "price_entry", ID: v.PriceID, K: proj.KSet, F: map[string]any{"price_paise": v.PricePaise}, Prev: prevStamp(cur, "price_paise")})
		} else {
			ms = append(ms, proj.Mut{E: "price_entry", ID: v.PriceID, K: proj.KInsert, F: map[string]any{"item_id": it.ID, "variant_id": v.ID, "price_paise": v.PricePaise}})
		}
	}
	return ms
}
