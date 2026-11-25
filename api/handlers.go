package api

import (
	"net/http"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/page"
	"github.com/hostryhq/svc-catalog/service"
)

func nextCursor(next string) *string {
	if next == "" {
		return nil
	}
	c := page.Encode(next)
	return &c
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}

// input validates the paging parameters and builds the service input.
func (s *Server) input(w http.ResponseWriter, r *http.Request, outlet *string, cursor *string, limit *int) (service.ListInput, bool) {
	after, n, err := pageParams(cursor, limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return service.ListInput{}, false
	}
	return service.ListInput{Outlet: deref(outlet), After: after, Limit: n}, true
}

// list maps a service page to a generated page body.
func list[S, D any](p service.Page[S], err error, conv func(S) D) ([]D, *string, error) {
	items := make([]D, 0, len(p.Items))
	for _, x := range p.Items {
		items = append(items, conv(x))
	}
	return items, nextCursor(p.Next), err
}

func (s *Server) ListCategories(w http.ResponseWriter, r *http.Request, p gen.ListCategoriesParams) {
	in, ok := s.input(w, r, nil, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListCategories(r.Context(), in, string(deref(p.Status)))
	items, next, err := list(pg, err, categoryDTO)
	s.reply(w, r, http.StatusOK, gen.CategoryPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListItems(w http.ResponseWriter, r *http.Request, p gen.ListItemsParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListItems(r.Context(), service.ListItemsInput{ListInput: in, CategoryID: deref(p.CategoryId), Status: string(deref(p.Status)), Q: deref(p.Q)})
	items, next, err := list(pg, err, itemDTO)
	s.reply(w, r, http.StatusOK, gen.ItemPage{Items: items, NextCursor: next}, err)
}

func (s *Server) GetItem(w http.ResponseWriter, r *http.Request, itemID gen.Id, p gen.GetItemParams) {
	id, ok := s.pathID(w, r, itemID)
	if !ok {
		return
	}
	it, err := s.svc.GetItem(r.Context(), id, deref(p.XOutletId))
	s.reply(w, r, http.StatusOK, itemDTO(it), err)
}

func (s *Server) ListModifierGroups(w http.ResponseWriter, r *http.Request, p gen.ListModifierGroupsParams) {
	in, ok := s.input(w, r, nil, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListModifierGroups(r.Context(), in)
	items, next, err := list(pg, err, groupDTO)
	s.reply(w, r, http.StatusOK, gen.ModifierGroupPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListReasonCodes(w http.ResponseWriter, r *http.Request, p gen.ListReasonCodesParams) {
	in, ok := s.input(w, r, nil, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListReasonCodes(r.Context(), in, string(deref(p.Purpose)))
	items, next, err := list(pg, err, reasonDTO)
	s.reply(w, r, http.StatusOK, gen.ReasonCodePage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListStations(w http.ResponseWriter, r *http.Request, p gen.ListStationsParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListStations(r.Context(), in)
	items, next, err := list(pg, err, stationDTO)
	s.reply(w, r, http.StatusOK, gen.StationPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListFloors(w http.ResponseWriter, r *http.Request, p gen.ListFloorsParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListFloors(r.Context(), in)
	items, next, err := list(pg, err, floorDTO)
	s.reply(w, r, http.StatusOK, gen.FloorPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListDiningTables(w http.ResponseWriter, r *http.Request, p gen.ListDiningTablesParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListDiningTables(r.Context(), in)
	items, next, err := list(pg, err, tableDTO)
	s.reply(w, r, http.StatusOK, gen.DiningTablePage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListPrinters(w http.ResponseWriter, r *http.Request, p gen.ListPrintersParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListPrinters(r.Context(), in)
	items, next, err := list(pg, err, printerDTO)
	s.reply(w, r, http.StatusOK, gen.PrinterPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListItemAvailability(w http.ResponseWriter, r *http.Request, p gen.ListItemAvailabilityParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	var avail *bool
	if p.Available != nil {
		b := *p.Available == "true"
		avail = &b
	}
	pg, err := s.svc.ListAvailability(r.Context(), in, avail)
	items, next, err := list(pg, err, overlayDTO)
	s.reply(w, r, http.StatusOK, gen.ItemOutletPage{Items: items, NextCursor: next}, err)
}

func (s *Server) ListCatalogExceptions(w http.ResponseWriter, r *http.Request, p gen.ListCatalogExceptionsParams) {
	in, ok := s.input(w, r, p.XOutletId, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListExceptions(r.Context(), in, string(deref(p.Status)))
	items, next, err := list(pg, err, exceptionDTO)
	s.reply(w, r, http.StatusOK, gen.ExceptionPage{Items: items, NextCursor: next}, err)
}

func (s *Server) GetMenu(w http.ResponseWriter, r *http.Request, p gen.GetMenuParams) {
	m, err := s.svc.GetMenu(r.Context(), deref(p.XOutletId))
	out := gen.Menu{BrandId: m.BrandID, OutletId: m.OutletID, GeneratedAt: m.GeneratedAt, Categories: []gen.Category{}, Items: []gen.Item{},
		ModifierGroups: []gen.ModifierGroup{}, Availability: []gen.ItemOutlet{}}
	for _, c := range m.Categories {
		out.Categories = append(out.Categories, categoryDTO(c))
	}
	for _, i := range m.Items {
		out.Items = append(out.Items, itemDTO(i))
	}
	for _, g := range m.ModifierGroups {
		out.ModifierGroups = append(out.ModifierGroups, groupDTO(g))
	}
	for _, a := range m.Availability {
		out.Availability = append(out.Availability, overlayDTO(a))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) SetCategoryTaxClass(w http.ResponseWriter, r *http.Request, categoryID gen.Id, _ gen.SetCategoryTaxClassParams) {
	s.setTax(w, r, categoryID, "setCategoryTaxClass", s.svc.SetCategoryTaxClass)
}

func (s *Server) SetItemTaxClass(w http.ResponseWriter, r *http.Request, itemID gen.Id, _ gen.SetItemTaxClassParams) {
	s.setTax(w, r, itemID, "setItemTaxClass", s.svc.SetItemTaxClass)
}

func (s *Server) setTax(w http.ResponseWriter, r *http.Request, rawID, op string, do func(ctxT, ids.ID, string, string) (service.TaxChange, error)) {
	id, ok := s.pathID(w, r, rawID)
	if !ok {
		return
	}
	var in gen.SetTaxClass
	ctx, ok := s.begin(w, r, op, &in)
	if !ok {
		return
	}
	c, err := do(ctx, id, in.TaxClass, in.Reason)
	s.reply(w, r, http.StatusOK, gen.TaxClassChange{Id: c.ID, TaxClass: c.TaxClass, UpdatedAt: c.UpdatedAt, OpId: c.OpID}, err)
}

func (s *Server) CreateMenuImport(w http.ResponseWriter, r *http.Request, _ gen.CreateMenuImportParams) {
	var in gen.CreateMenuImport
	ctx, ok := s.begin(w, r, "createMenuImport", &in)
	if !ok {
		return
	}
	im, err := s.svc.CreateMenuImport(ctx, in.Filename, in.CsvText)
	s.reply(w, r, http.StatusCreated, importDTO(im), err)
}

func (s *Server) ListMenuImports(w http.ResponseWriter, r *http.Request, p gen.ListMenuImportsParams) {
	in, ok := s.input(w, r, nil, p.Cursor, p.Limit)
	if !ok {
		return
	}
	pg, err := s.svc.ListMenuImports(r.Context(), in.After, in.Limit)
	items, next, err := list(pg, err, importDTO)
	s.reply(w, r, http.StatusOK, gen.MenuImportPage{Items: items, NextCursor: next}, err)
}

func (s *Server) GetMenuImport(w http.ResponseWriter, r *http.Request, importID gen.Id) {
	id, ok := s.pathID(w, r, importID)
	if !ok {
		return
	}
	im, err := s.svc.GetMenuImport(r.Context(), id)
	s.reply(w, r, http.StatusOK, importDTO(im), err)
}

func (s *Server) CancelMenuImport(w http.ResponseWriter, r *http.Request, importID gen.Id, _ gen.CancelMenuImportParams) {
	id, ok := s.pathID(w, r, importID)
	if !ok {
		return
	}
	ctx, ok := s.begin(w, r, "cancelMenuImport", nil)
	if !ok {
		return
	}
	im, err := s.svc.CancelMenuImport(ctx, id)
	s.reply(w, r, http.StatusOK, importDTO(im), err)
}

func (s *Server) CommitMenuImport(w http.ResponseWriter, r *http.Request, importID gen.Id, _ gen.CommitMenuImportParams) {
	id, ok := s.pathID(w, r, importID)
	if !ok {
		return
	}
	var in gen.CommitMenuImport
	ctx, ok := s.begin(w, r, "commitMenuImport", &in)
	if !ok {
		return
	}
	im, err := s.svc.CommitMenuImport(ctx, id, in.RowCount)
	s.reply(w, r, http.StatusOK, importDTO(im), err)
}
