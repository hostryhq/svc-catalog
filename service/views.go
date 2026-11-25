package service

import (
	"sort"
	"time"

	"github.com/hostryhq/go-auth"

	"github.com/hostryhq/svc-catalog/internal/model"
)

var allOutlets = auth.AllOutlets()

// Typed views of the projected rows, as the API presents them (api-specs openapi/catalog/v1).

// CategoryView is a menu category.
type CategoryView struct {
	ID, BrandID, NameEn, NameTa, NameMl string
	DefaultStationCode                  string
	DefaultTaxClass                     string
	SortOrder                           int
	Status                              string
}

// VariantView is a named price point of an item.
type VariantView struct {
	ID, ItemID, Name string
	PricePaise       int64
	IsDefault        bool
	SortOrder        int
}

// ItemView is a menu item with its variants.
type ItemView struct {
	ID, BrandID, CategoryID, NameEn, NameTa, NameMl string
	KitchenName, StationCode, VegMark, TaxClass     string
	Code                                            string
	Status                                          string
	SortOrder                                       int
	Variants                                        []VariantView
	ModifierGroupIDs                                []string
}

// ModifierView is a modifier option.
type ModifierView struct {
	ID, Name   string
	DeltaPaise int64
	SortOrder  int
}

// ModifierGroupView is a modifier group with its options.
type ModifierGroupView struct {
	ID, Name             string
	MinSelect, MaxSelect int
	Modifiers            []ModifierView
}

// OverlayView is the per-outlet overlay of an item.
type OverlayView struct {
	RowID, ItemID, OutletID string
	Available               bool
	OutletPrice             *int64
	StationID               string
	QuickItem               bool
}

// StationView is a kitchen or bar station.
type StationView struct {
	ID, OutletID, Code, Name, Output, PrinterID, Language string
}

// FloorView is a floor section.
type FloorView struct {
	ID, OutletID, Name string
	SortOrder          int
}

// TableView is a dining table.
type TableView struct {
	ID, OutletID, FloorID, Name string
	Seats                       int
	Active                      bool
}

// PrinterView is a printer and how it is reached.
type PrinterView struct {
	ID, OutletID, Name, Connection, Address, HostDeviceID, CodePage, PowerBackup string
	Port                                                                         *int
	PaperWidthMM, CharsPerLine, DrawerPin                                        int
	Cut, Raster                                                                  bool
}

// ReasonView is a reason code.
type ReasonView struct {
	ID, Purpose, LabelKey string
	Active                bool
}

// MenuView is the whole sellable menu of a brand with the outlet overlay.
type MenuView struct {
	BrandID, OutletID string
	GeneratedAt       time.Time
	Categories        []CategoryView
	Items             []ItemView
	ModifierGroups    []ModifierGroupView
	Availability      []OverlayView
}

// TaxChange is the result of a tax-class change.
type TaxChange struct {
	ID        string    `json:"id"`
	TaxClass  string    `json:"tax_class"`
	UpdatedAt time.Time `json:"updated_at"`
	OpID      string    `json:"op_id"`
}

func status(r model.Row) string {
	if r.Gone {
		return "archived"
	}
	if s := r.Str("status"); s != "" {
		return s
	}
	return "active"
}

func categoryView(r model.Row) CategoryView {
	return CategoryView{ID: r.ID, BrandID: r.ScopeID.String(), NameEn: r.Str("name_en"), NameTa: r.Str("name_ta"), NameMl: r.Str("name_ml"),
		DefaultStationCode: r.Str("default_station_code"), DefaultTaxClass: r.Str("default_tax_class"), SortOrder: int(r.Int("sort_order")), Status: status(r)}
}

func itemBase(r model.Row) ItemView {
	kn := r.Str("kitchen_name")
	if kn == "" {
		kn = r.Str("name_en")
	}
	return ItemView{ID: r.ID, BrandID: r.ScopeID.String(), CategoryID: r.Str("category_id"), NameEn: r.Str("name_en"), NameTa: r.Str("name_ta"),
		NameMl: r.Str("name_ml"), KitchenName: kn, StationCode: r.Str("station_code"), VegMark: orDefault(r.Str("veg_mark"), "none"),
		TaxClass: r.Str("tax_class"), Code: r.Str("code"), Status: status(r), SortOrder: int(r.Int("sort_order")),
		Variants: []VariantView{}, ModifierGroupIDs: []string{}}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func overlayView(r model.Row) OverlayView {
	o := OverlayView{RowID: r.ID, ItemID: r.Str("item_id"), OutletID: r.ScopeID.String(), Available: true, StationID: r.Str("station_id"), QuickItem: r.Bool("quick_item")}
	if r.Has("available") {
		o.Available = r.Bool("available")
	}
	if r.Has("outlet_price_paise") {
		p := r.Int("outlet_price_paise")
		o.OutletPrice = &p
	}
	return o
}

func stationView(r model.Row) StationView {
	return StationView{ID: r.ID, OutletID: r.ScopeID.String(), Code: r.Str("code"), Name: r.Str("name"), Output: r.Str("output"),
		PrinterID: r.Str("printer_id"), Language: orDefault(r.Str("language"), "en")}
}

func printerView(r model.Row) PrinterView {
	p := PrinterView{ID: r.ID, OutletID: r.ScopeID.String(), Name: r.Str("name"), Connection: r.Str("connection"), Address: r.Str("address"),
		HostDeviceID: r.Str("host_device_id"), CodePage: r.Str("code_page"), PowerBackup: orDefault(r.Str("power_backup"), "none"),
		PaperWidthMM: int(r.Int("paper_width_mm")), CharsPerLine: int(r.Int("chars_per_line")), DrawerPin: int(r.Int("drawer_pin")),
		Cut: r.Bool("cut"), Raster: r.Bool("raster")}
	if r.Has("port") {
		n := int(r.Int("port"))
		p.Port = &n
	}
	return p
}

func sortVariants(vs []VariantView) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].IsDefault != vs[j].IsDefault {
			return vs[i].IsDefault
		}
		if vs[i].SortOrder != vs[j].SortOrder {
			return vs[i].SortOrder < vs[j].SortOrder
		}
		return vs[i].ID < vs[j].ID
	})
}
