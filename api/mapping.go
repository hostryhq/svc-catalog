package api

import (
	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/service"
)

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func money(p int64) gen.Money { return gen.Money{AmountPaise: p, Currency: gen.INR} }

func categoryDTO(c service.CategoryView) gen.Category {
	return gen.Category{Id: c.ID, BrandId: c.BrandID, NameEn: c.NameEn, NameTa: opt(c.NameTa), NameMl: opt(c.NameMl),
		DefaultStationCode: opt(c.DefaultStationCode), DefaultTaxClass: c.DefaultTaxClass, SortOrder: c.SortOrder, Status: gen.CatalogStatus(c.Status)}
}

func itemDTO(i service.ItemView) gen.Item {
	vs := make([]gen.ItemVariant, 0, len(i.Variants))
	for _, v := range i.Variants {
		vs = append(vs, gen.ItemVariant{Id: v.ID, ItemId: v.ItemID, Name: v.Name, Price: money(v.PricePaise), IsDefault: v.IsDefault, SortOrder: v.SortOrder})
	}
	return gen.Item{Id: i.ID, BrandId: i.BrandID, CategoryId: i.CategoryID, NameEn: i.NameEn, NameTa: opt(i.NameTa), NameMl: opt(i.NameMl),
		KitchenName: i.KitchenName, StationCode: opt(i.StationCode), VegMark: gen.VegMark(i.VegMark), TaxClass: i.TaxClass, Code: opt(i.Code),
		Status: gen.CatalogStatus(i.Status), SortOrder: i.SortOrder, Variants: vs, ModifierGroupIds: append([]string{}, i.ModifierGroupIDs...)}
}

func groupDTO(g service.ModifierGroupView) gen.ModifierGroup {
	ms := make([]gen.Modifier, 0, len(g.Modifiers))
	for _, m := range g.Modifiers {
		ms = append(ms, gen.Modifier{Id: m.ID, Name: m.Name, PriceDelta: money(m.DeltaPaise), SortOrder: m.SortOrder})
	}
	return gen.ModifierGroup{Id: g.ID, Name: g.Name, MinSelect: g.MinSelect, MaxSelect: g.MaxSelect, Modifiers: ms}
}

func overlayDTO(o service.OverlayView) gen.ItemOutlet {
	out := gen.ItemOutlet{ItemId: o.ItemID, OutletId: o.OutletID, Available: o.Available, QuickItem: o.QuickItem, StationId: opt(o.StationID)}
	if o.OutletPrice != nil {
		m := money(*o.OutletPrice)
		out.OutletPrice = &m
	}
	return out
}

func stationDTO(s service.StationView) gen.Station {
	return gen.Station{Id: s.ID, OutletId: s.OutletID, Code: s.Code, Name: s.Name, Output: gen.StationOutput(s.Output), PrinterId: opt(s.PrinterID), Language: gen.StationLanguage(s.Language)}
}

func floorDTO(f service.FloorView) gen.Floor {
	return gen.Floor{Id: f.ID, OutletId: f.OutletID, Name: f.Name, SortOrder: f.SortOrder}
}

func tableDTO(t service.TableView) gen.DiningTable {
	return gen.DiningTable{Id: t.ID, OutletId: t.OutletID, FloorId: t.FloorID, Name: t.Name, Seats: t.Seats, Active: t.Active}
}

func printerDTO(p service.PrinterView) gen.Printer {
	return gen.Printer{Id: p.ID, OutletId: p.OutletID, Name: p.Name, Connection: gen.PrinterConnection(p.Connection), Address: opt(p.Address), Port: p.Port,
		HostDeviceId: opt(p.HostDeviceID), PaperWidthMm: p.PaperWidthMM, CharsPerLine: p.CharsPerLine, CodePage: p.CodePage, Cut: p.Cut,
		DrawerPin: p.DrawerPin, Raster: p.Raster, PowerBackup: gen.PrinterPowerBackup(p.PowerBackup)}
}

func reasonDTO(r service.ReasonView) gen.ReasonCode {
	return gen.ReasonCode{Id: r.ID, Purpose: gen.ReasonCodePurpose(r.Purpose), LabelKey: r.LabelKey, Active: r.Active}
}

func exceptionDTO(e model.Exception) gen.Exception {
	out := gen.Exception{Id: e.ID.String(), Kind: e.Kind, Severity: gen.ExceptionSeverity(e.Severity), Status: gen.ExceptionStatus(e.Status),
		SubjectKind: e.SubjectKind, SubjectId: e.SubjectID, OutletId: e.OutletID.String(), DetectedAt: e.DetectedAt, DetailKey: e.DetailKey,
		ResolvedAt: e.ResolvedAt, ResolvedByRef: opt(e.ResolvedRef)}
	if len(e.Params) > 0 {
		p := map[string]any{}
		for k, v := range e.Params {
			p[k] = v
		}
		out.Params = &p
	}
	return out
}

func importDTO(im model.Import) gen.MenuImport {
	errs := make([]gen.ImportRowError, 0, len(im.Errors))
	for _, e := range im.Errors {
		errs = append(errs, gen.ImportRowError{Row: e.Row, Field: e.Field, Code: e.Code})
	}
	return gen.MenuImport{Id: im.ID.String(), Status: gen.MenuImportStatus(im.Status), Source: gen.MenuImportSource(im.Source), Filename: im.Filename,
		RowCount: im.RowCount, ErrorCount: im.ErrorCount, Errors: errs, CategoriesCreated: im.CategoriesCreated, ItemsCreated: im.ItemsCreated,
		ItemsUpdated: im.ItemsUpdated, PriceChanges: im.PriceChanges, CreatedAt: im.CreatedAt, CommittedAt: im.CommittedAt}
}
