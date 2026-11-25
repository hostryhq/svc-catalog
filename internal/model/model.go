// Package model holds the domain types of the catalog service and its typed errors. It has no HTTP and no
// SQL. Rows of synced entities are stored generically (see Row); the typed views (Item, Category, ...) are
// built from them by the service.
package model

import (
	"errors"
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"
)

// ErrNotFound is the one answer for a missing, foreign-tenant or out-of-scope resource.
var ErrNotFound = apperr.New(apperr.NotFound, "not_found", "")

// ErrRepoNotFound is returned by repositories for a missing row; the service maps it to ErrNotFound.
var ErrRepoNotFound = errors.New("model: row not found")

// Invalid builds a 422 with one field error.
func Invalid(field, code, msg string) error {
	return apperr.New(apperr.Validation, "validation", "invalid request").WithFields(apperr.FieldError{Field: field, Code: code, Message: msg})
}

// Stream kinds of synced rows (ADR-0002 S3). The tenant stream carries only rows catalog does not own.
const (
	StreamBrand  = "brand"
	StreamOutlet = "outlet"
)

// OutletRef is what catalog knows about an outlet, projected from tenancy's outlet events: its brand decides
// which menu the outlet sells.
type OutletRef struct {
	ID      ids.ID
	BrandID ids.ID
	Kind    string
	Status  string
}

// Row is one projected row of a synced entity: the visible fields derived from the op effects (see
// internal/proj). ScopeID is the brand id (brand stream) or the outlet id (outlet stream).
type Row struct {
	Entity  string
	ID      string
	Stream  string
	ScopeID ids.ID
	Fields  map[string]any
	// Stamps maps each field to the hybrid-clock stamp (16 hex characters) of the winning write. A later edit
	// of a review field cites it as `prev`.
	Stamps    map[string]string
	Gone      bool // tombstoned (archived)
	UpdatedAt time.Time
}

// Str returns a string field.
func (r Row) Str(f string) string { s, _ := r.Fields[f].(string); return s }

// Int returns an integer field (zero when absent).
func (r Row) Int(f string) int64 {
	switch v := r.Fields[f].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	}
	return 0
}

// Bool returns a boolean field.
func (r Row) Bool(f string) bool { b, _ := r.Fields[f].(bool); return b }

// Has reports whether the field is present.
func (r Row) Has(f string) bool { _, ok := r.Fields[f]; return ok }

// Exception kinds of catalog (api-specs openapi/catalog: listCatalogExceptions).
const (
	KindPriceConflict = "price_conflict"
	StatusOpen        = "open"
	StatusSuperseded  = "superseded"
	StatusResolved    = "resolved"
)

// Exception is a detector finding derived from the projection.
type Exception struct {
	ID          ids.ID
	Kind        string
	Severity    string
	Status      string
	SubjectKind string
	SubjectID   string
	OutletID    ids.ID
	DetectedAt  time.Time
	ResolvedAt  *time.Time
	ResolvedRef string
	DetailKey   string
	Params      map[string]any
	// DedupeKey makes re-derivation idempotent: one finding per conflicting write and outlet.
	DedupeKey string
}

// Import job statuses and sources.
const (
	ImportPreviewed = "previewed"
	ImportCommitted = "committed"
	ImportCancelled = "cancelled"
	SourceOwner     = "owner"
	SourceSupport   = "support_session"
)

// ImportRowError is one row-level error of a preview.
type ImportRowError struct {
	Row   int    `json:"row"`
	Field string `json:"field"`
	Code  string `json:"code"`
}

// PlanVariant, PlanItem and PlanCategory are the ops a valid import will emit, frozen at preview time (ids
// included) so a retried commit emits the same rows.
type PlanVariant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PricePaise int64  `json:"price_paise"`
	PriceID    string `json:"price_id"`
	Default    bool   `json:"default"`
	Sort       int    `json:"sort"`
	Exists     bool   `json:"exists"` // an existing variant matched by name: its price is a price edit
}

// PlanItem is one item of the plan.
type PlanItem struct {
	ID         string        `json:"id"`
	Exists     bool          `json:"exists"`
	Code       string        `json:"code,omitempty"`
	NameEn     string        `json:"name_en"`
	KitchenEn  string        `json:"kitchen_name"`
	CategoryID string        `json:"category_id"`
	Station    string        `json:"station,omitempty"`
	VegMark    string        `json:"veg_mark"`
	TaxClass   string        `json:"tax_class"`
	Sort       int           `json:"sort"`
	Variants   []PlanVariant `json:"variants"`
}

// PlanCategory is one category of the plan.
type PlanCategory struct {
	ID       string `json:"id"`
	Exists   bool   `json:"exists"`
	NameEn   string `json:"name_en"`
	Station  string `json:"station,omitempty"`
	TaxClass string `json:"tax_class"`
	Sort     int    `json:"sort"`
}

// ImportPlan is the preview's frozen result.
type ImportPlan struct {
	BrandID    string         `json:"brand_id"`
	Categories []PlanCategory `json:"categories"`
	Items      []PlanItem     `json:"items"`
}

// Import is a menu import job (server-only data, ADR-0014 R3).
type Import struct {
	ID                ids.ID
	Status            string
	Source            string
	Filename          string
	RowCount          int
	ErrorCount        int
	Errors            []ImportRowError // at most 100
	CategoriesCreated int
	ItemsCreated      int
	ItemsUpdated      int
	PriceChanges      int
	CreatedAt         time.Time
	CommittedAt       *time.Time
	CreatedBy         string
	Plan              *ImportPlan
}

// AuditEntry records a strong-session action: who, what, when and the values before and after
// (security-privacy: sensitive actions write an audit record). Ids and fixed values only.
type AuditEntry struct {
	ID          ids.ID
	At          time.Time
	Actor       string
	Action      string // for example menu.tax_class.edit, menu.import.commit
	SubjectKind string
	SubjectID   string
	Before      string
	After       string
	Reason      string
}
