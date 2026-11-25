package service

import (
	"context"
	"sort"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
)

// ListInput carries the paging and the optional outlet of a read.
type ListInput struct {
	Outlet string // X-Outlet-Id, "" when absent
	After  string // id to continue after
	Limit  int
}

func (in ListInput) fetch() int { return in.Limit + 1 }

func brandQuery(entity string, brands repository.BrandSet, in ListInput) repository.RowQuery {
	return repository.RowQuery{Entity: entity, Brands: brands, After: in.After, Limit: in.fetch()}
}

// ListCategories lists categories in sort order. Archived categories are listed only when asked for.
func (s *Service) ListCategories(ctx context.Context, in ListInput, statusFilter string) (Page[CategoryView], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[CategoryView], error) {
		sc, named, err := s.scope(ctx, tx, PermMenuRead, in.Outlet)
		if err != nil {
			return Page[CategoryView]{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, named)
		if err != nil {
			return Page[CategoryView]{}, err
		}
		q := brandQuery("category", brands, in)
		applyStatus(&q, statusFilter)
		rows, err := tx.ListRows(ctx, q)
		if err != nil {
			return Page[CategoryView]{}, err
		}
		var out []CategoryView
		for _, r := range rows {
			out = append(out, categoryView(r))
		}
		return pageOf(out, in.Limit, func(c CategoryView) string { return c.ID }), nil
	})
}

// applyStatus maps the status filter: archived is the tombstone, hidden and active are a field value.
func applyStatus(q *repository.RowQuery, st string) {
	switch st {
	case "archived":
		q.Gone = true
	case "":
	default:
		q.Eq = map[string]string{"status": st}
	}
}

// ListItemsInput adds the item filters.
type ListItemsInput struct {
	ListInput
	CategoryID, Status, Q string
}

// ListItems lists items with their variants and modifier groups, in sort order.
func (s *Service) ListItems(ctx context.Context, in ListItemsInput) (Page[ItemView], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[ItemView], error) {
		sc, named, err := s.scope(ctx, tx, PermMenuRead, in.Outlet)
		if err != nil {
			return Page[ItemView]{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, named)
		if err != nil {
			return Page[ItemView]{}, err
		}
		q := brandQuery("item", brands, in.ListInput)
		applyStatus(&q, in.Status)
		if in.CategoryID != "" {
			q.Eq = mergeEq(q.Eq, "category_id", in.CategoryID)
		}
		if in.Q != "" {
			q.Search, q.SearchFields, q.ExactField = in.Q, []string{"name_en", "name_ta", "name_ml", "kitchen_name"}, "code"
		}
		rows, err := tx.ListRows(ctx, q)
		if err != nil {
			return Page[ItemView]{}, err
		}
		page, more := paginate(rows, in.Limit)
		items, err := s.itemViews(ctx, tx, brands, page)
		if err != nil {
			return Page[ItemView]{}, err
		}
		p := Page[ItemView]{Items: items}
		if more && len(items) > 0 {
			p.Next = items[len(items)-1].ID
		}
		return p, nil
	})
}

func mergeEq(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// GetItem returns one item (archived ones too: history keeps reading them).
func (s *Service) GetItem(ctx context.Context, id ids.ID, outlet string) (ItemView, error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (ItemView, error) {
		sc, named, err := s.scope(ctx, tx, PermMenuRead, outlet)
		if err != nil {
			return ItemView{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, named)
		if err != nil {
			return ItemView{}, err
		}
		r, err := tx.GetRow(ctx, repository.RowQuery{Entity: "item", Brands: brands}, id.String())
		if err != nil {
			return ItemView{}, repoErr(err)
		}
		items, err := s.itemViews(ctx, tx, brands, []model.Row{r})
		if err != nil {
			return ItemView{}, err
		}
		return items[0], nil
	})
}

// itemViews attaches variants (with their prices) and modifier group links to a page of item rows.
func (s *Service) itemViews(ctx context.Context, tx repository.Tx, brands repository.BrandSet, rows []model.Row) ([]ItemView, error) {
	if len(rows) == 0 {
		return []ItemView{}, nil
	}
	itemIDs := make([]string, len(rows))
	for i, r := range rows {
		itemIDs[i] = r.ID
	}
	in := map[string][]string{"item_id": itemIDs}
	variants, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item_variant", Brands: brands, In: in})
	if err != nil {
		return nil, err
	}
	prices, err := tx.ListRows(ctx, repository.RowQuery{Entity: "price_entry", Brands: brands, In: in})
	if err != nil {
		return nil, err
	}
	links, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item_modifier_group", Brands: brands, In: in})
	if err != nil {
		return nil, err
	}
	type pk struct{ item, variant string }
	price := map[pk]int64{}
	for _, p := range prices {
		price[pk{p.Str("item_id"), p.Str("variant_id")}] = p.Int("price_paise")
	}
	byItem := map[string][]VariantView{}
	for _, v := range variants {
		byItem[v.Str("item_id")] = append(byItem[v.Str("item_id")], VariantView{ID: v.ID, ItemID: v.Str("item_id"), Name: v.Str("name"),
			PricePaise: price[pk{v.Str("item_id"), v.ID}], IsDefault: v.Bool("is_default"), SortOrder: int(v.Int("sort_order"))})
	}
	groups := map[string][]model.Row{}
	for _, l := range links {
		groups[l.Str("item_id")] = append(groups[l.Str("item_id")], l)
	}
	out := make([]ItemView, len(rows))
	for i, r := range rows {
		iv := itemBase(r)
		iv.Variants = byItem[r.ID]
		if len(iv.Variants) == 0 {
			// A priced item without variants has one default variant named after the item (RM-M2); its id is
			// the item's own id.
			if p, ok := price[pk{r.ID, ""}]; ok {
				iv.Variants = []VariantView{{ID: r.ID, ItemID: r.ID, Name: r.Str("name_en"), PricePaise: p, IsDefault: true}}
			}
		}
		if iv.Variants == nil {
			iv.Variants = []VariantView{}
		}
		sortVariants(iv.Variants)
		ls := groups[r.ID]
		sort.SliceStable(ls, func(a, b int) bool {
			if ls[a].Int("sort_order") != ls[b].Int("sort_order") {
				return ls[a].Int("sort_order") < ls[b].Int("sort_order")
			}
			return ls[a].ID < ls[b].ID
		})
		for _, l := range ls {
			iv.ModifierGroupIDs = append(iv.ModifierGroupIDs, l.Str("group_id"))
		}
		out[i] = iv
	}
	return out, nil
}

// ListModifierGroups lists modifier groups with their options.
func (s *Service) ListModifierGroups(ctx context.Context, in ListInput) (Page[ModifierGroupView], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[ModifierGroupView], error) {
		sc, named, err := s.scope(ctx, tx, PermMenuRead, in.Outlet)
		if err != nil {
			return Page[ModifierGroupView]{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, named)
		if err != nil {
			return Page[ModifierGroupView]{}, err
		}
		rows, err := tx.ListRows(ctx, brandQuery("modifier_group", brands, in))
		if err != nil {
			return Page[ModifierGroupView]{}, err
		}
		page, more := paginate(rows, in.Limit)
		gs, err := s.groupViews(ctx, tx, brands, page)
		if err != nil {
			return Page[ModifierGroupView]{}, err
		}
		p := Page[ModifierGroupView]{Items: gs}
		if more && len(gs) > 0 {
			p.Next = gs[len(gs)-1].ID
		}
		return p, nil
	})
}

func (s *Service) groupViews(ctx context.Context, tx repository.Tx, brands repository.BrandSet, rows []model.Row) ([]ModifierGroupView, error) {
	out := []ModifierGroupView{}
	if len(rows) == 0 {
		return out, nil
	}
	gids := make([]string, len(rows))
	for i, r := range rows {
		gids[i] = r.ID
	}
	mods, err := tx.ListRows(ctx, repository.RowQuery{Entity: "modifier", Brands: brands, In: map[string][]string{"group_id": gids}})
	if err != nil {
		return nil, err
	}
	by := map[string][]ModifierView{}
	for _, m := range mods {
		by[m.Str("group_id")] = append(by[m.Str("group_id")], ModifierView{ID: m.ID, Name: m.Str("name"), DeltaPaise: m.Int("price_delta_paise"), SortOrder: int(m.Int("sort_order"))})
	}
	for _, r := range rows {
		ms := by[r.ID]
		sort.SliceStable(ms, func(a, b int) bool {
			if ms[a].SortOrder != ms[b].SortOrder {
				return ms[a].SortOrder < ms[b].SortOrder
			}
			return ms[a].ID < ms[b].ID
		})
		if ms == nil {
			ms = []ModifierView{}
		}
		out = append(out, ModifierGroupView{ID: r.ID, Name: r.Str("name"), MinSelect: int(r.Int("min_select")), MaxSelect: int(r.Int("max_select")), Modifiers: ms})
	}
	return out, nil
}

// ListReasonCodes lists the brand's reason codes, optionally of one purpose.
func (s *Service) ListReasonCodes(ctx context.Context, in ListInput, purpose string) (Page[ReasonView], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[ReasonView], error) {
		sc, named, err := s.scope(ctx, tx, PermMenuRead, in.Outlet)
		if err != nil {
			return Page[ReasonView]{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, named)
		if err != nil {
			return Page[ReasonView]{}, err
		}
		q := brandQuery("reason_code", brands, in)
		if purpose != "" {
			q.Eq = map[string]string{"purpose": purpose}
		}
		rows, err := tx.ListRows(ctx, q)
		if err != nil {
			return Page[ReasonView]{}, err
		}
		var out []ReasonView
		for _, r := range rows {
			active := true
			if r.Has("active") {
				active = r.Bool("active")
			}
			out = append(out, ReasonView{ID: r.ID, Purpose: r.Str("purpose"), LabelKey: r.Str("label_key"), Active: active})
		}
		return pageOf(out, in.Limit, func(v ReasonView) string { return v.ID }), nil
	})
}

// outletRows runs an outlet-scoped list of one entity and maps each row.
func outletRows[T any](ctx context.Context, s *Service, in ListInput, perm, entity string, tweak func(*repository.RowQuery), view func(model.Row) T, id func(T) string) (Page[T], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[T], error) {
		sc, _, err := s.scope(ctx, tx, perm, in.Outlet)
		if err != nil {
			return Page[T]{}, err
		}
		q := repository.RowQuery{Entity: entity, Scope: sc, After: in.After, Limit: in.fetch()}
		if tweak != nil {
			tweak(&q)
		}
		rows, err := tx.ListRows(ctx, q)
		if err != nil {
			return Page[T]{}, err
		}
		out := make([]T, 0, len(rows))
		for _, r := range rows {
			out = append(out, view(r))
		}
		return pageOf(out, in.Limit, id), nil
	})
}

// ListStations lists the outlet's stations.
func (s *Service) ListStations(ctx context.Context, in ListInput) (Page[StationView], error) {
	return outletRows(ctx, s, in, PermSettingsRead, "station", nil, stationView, func(v StationView) string { return v.ID })
}

// ListFloors lists the outlet's floors.
func (s *Service) ListFloors(ctx context.Context, in ListInput) (Page[FloorView], error) {
	return outletRows(ctx, s, in, PermSettingsRead, "floor", nil, func(r model.Row) FloorView {
		return FloorView{ID: r.ID, OutletID: r.ScopeID.String(), Name: r.Str("name"), SortOrder: int(r.Int("sort_order"))}
	}, func(v FloorView) string { return v.ID })
}

// ListDiningTables lists the outlet's tables.
func (s *Service) ListDiningTables(ctx context.Context, in ListInput) (Page[TableView], error) {
	return outletRows(ctx, s, in, PermSettingsRead, "dining_table", nil, func(r model.Row) TableView {
		active := true
		if r.Has("active") {
			active = r.Bool("active")
		}
		return TableView{ID: r.ID, OutletID: r.ScopeID.String(), FloorID: r.Str("floor_id"), Name: r.Str("name"), Seats: int(r.Int("seats")), Active: active}
	}, func(v TableView) string { return v.ID })
}

// ListPrinters lists the outlet's printers.
func (s *Service) ListPrinters(ctx context.Context, in ListInput) (Page[PrinterView], error) {
	return outletRows(ctx, s, in, PermSettingsRead, "printer", nil, printerView, func(v PrinterView) string { return v.ID })
}

// ListAvailability lists the outlet overlay rows, optionally only sold-out (available=false) or available ones.
func (s *Service) ListAvailability(ctx context.Context, in ListInput, available *bool) (Page[OverlayView], error) {
	return outletRows(ctx, s, in, PermMenuRead, "item_outlet", func(q *repository.RowQuery) {
		if available != nil {
			q.BoolEq = map[string]bool{"available": *available}
		}
	}, overlayView, func(v OverlayView) string { return v.RowID })
}

// ExceptionView is a detector finding.
type ExceptionView = model.Exception

// ListExceptions lists the findings of the caller's outlets, newest first.
func (s *Service) ListExceptions(ctx context.Context, in ListInput, status string) (Page[ExceptionView], error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[ExceptionView], error) {
		sc, _, err := s.scope(ctx, tx, PermExceptions, in.Outlet)
		if err != nil {
			return Page[ExceptionView]{}, err
		}
		rows, err := tx.ListExceptions(ctx, repository.ExceptionQuery{Scope: sc, Status: status, After: ids.ID(in.After), Limit: in.fetch()})
		if err != nil {
			return Page[ExceptionView]{}, err
		}
		return pageOf(rows, in.Limit, func(e model.Exception) string { return e.ID.String() }), nil
	})
}

// GetMenu returns the whole sellable menu of the outlet's brand with the outlet overlay. Archived rows are
// excluded. Without X-Outlet-Id it needs a caller whose scope is exactly one outlet.
func (s *Service) GetMenu(ctx context.Context, outlet string) (MenuView, error) {
	return read(ctx, s, func(ctx context.Context, tx repository.Tx) (MenuView, error) {
		sc, _, err := s.scope(ctx, tx, PermMenuRead, outlet)
		if err != nil {
			return MenuView{}, err
		}
		o, err := s.menuOutlet(ctx, tx, sc, outlet)
		if err != nil {
			return MenuView{}, err
		}
		brands := repository.BrandSet{IDs: []ids.ID{o.BrandID}}
		var m MenuView
		m.BrandID, m.OutletID, m.GeneratedAt = o.BrandID.String(), o.ID.String(), s.clock.Now().UTC()
		cats, err := tx.ListRows(ctx, repository.RowQuery{Entity: "category", Brands: brands})
		if err != nil {
			return MenuView{}, err
		}
		m.Categories = []CategoryView{}
		for _, c := range cats {
			m.Categories = append(m.Categories, categoryView(c))
		}
		items, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item", Brands: brands})
		if err != nil {
			return MenuView{}, err
		}
		if m.Items, err = s.itemViews(ctx, tx, brands, items); err != nil {
			return MenuView{}, err
		}
		seen := map[string]bool{}
		for _, it := range m.Items {
			for _, g := range it.ModifierGroupIDs {
				seen[g] = true
			}
		}
		groups, err := tx.ListRows(ctx, repository.RowQuery{Entity: "modifier_group", Brands: brands})
		if err != nil {
			return MenuView{}, err
		}
		var used []model.Row
		for _, g := range groups {
			if seen[g.ID] {
				used = append(used, g)
			}
		}
		if m.ModifierGroups, err = s.groupViews(ctx, tx, brands, used); err != nil {
			return MenuView{}, err
		}
		ov, err := tx.ListRows(ctx, repository.RowQuery{Entity: "item_outlet", Scope: auth.OnlyOutlets(o.ID.String())})
		if err != nil {
			return MenuView{}, err
		}
		m.Availability = []OverlayView{}
		for _, r := range ov {
			m.Availability = append(m.Availability, overlayView(r))
		}
		return m, nil
	})
}

// menuOutlet picks the outlet a menu is read for.
func (s *Service) menuOutlet(ctx context.Context, tx repository.Tx, sc auth.OutletScope, named string) (model.OutletRef, error) {
	if named != "" {
		id, _ := ids.Parse(named)
		o, err := tx.GetOutlet(ctx, id)
		return o, repoErr(err)
	}
	os, err := tx.ListOutlets(ctx, sc)
	if err != nil {
		return model.OutletRef{}, err
	}
	if len(os) != 1 {
		return model.OutletRef{}, model.Invalid("X-Outlet-Id", "outlet_required", "name the outlet with X-Outlet-Id")
	}
	return os[0], nil
}
