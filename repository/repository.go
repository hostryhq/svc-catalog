// Package repository holds the persistence interfaces of the catalog service. The service depends on these
// interfaces only; repository/pg is the Postgres implementation and repository/mem the in-memory one used by
// the test double.
//
// Every Tx runs for exactly one tenant, taken from the tenancy context in ctx (go-common). The implementation
// applies it to the database (row-level security) and adds an explicit tenant predicate to every statement.
// Reads of outlet-scoped rows take the caller's auth.OutletScope and bind it as an outlet_id predicate
// (ADR-0003 A1.4); brand rows are reached through the brands of the caller's outlets.
package repository

import (
	"context"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/proj"
)

// Repository opens transactions.
type Repository interface {
	// InTx runs fn in one transaction for the tenant in ctx. fn's error rolls everything back. A request
	// made on a vendor user's authority (a support token) writes its vendor_access_log row in the same
	// transaction (go-auth WithAuditedTx).
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// Ping checks the database for readiness.
	Ping(ctx context.Context) error
}

// BrandSet selects brand rows: every brand of the tenant, or the listed ones.
type BrandSet struct {
	All bool
	IDs []ids.ID
}

// RowQuery selects rows of one entity.
type RowQuery struct {
	Entity string
	// Brands applies to brand-stream entities, Scope to outlet-stream entities.
	Brands BrandSet
	Scope  auth.OutletScope
	// Eq are exact matches on string fields; In are set matches (field value in the list).
	Eq map[string]string
	In map[string][]string
	// BoolEq are boolean matches where an absent field counts as true (availability defaults to available).
	BoolEq map[string]bool
	// Search is a case-insensitive substring over the SearchFields; Exact matches ExactField exactly.
	Search       string
	SearchFields []string
	ExactField   string
	// Gone selects tombstoned rows only (true) or live rows only (false).
	Gone bool
	// After is the id of the last row of the previous page; rows are ordered by sort key then id.
	After string
	Limit int
}

// EffectRec is a stored effect with the row it belongs to.
type EffectRec struct {
	Entity  string
	RowID   string
	Stream  string
	ScopeID ids.ID
	proj.Effect
}

// RowKey identifies one row.
type RowKey struct {
	Entity, ID, Stream string
	ScopeID            ids.ID
}

// ExceptionQuery selects exceptions inside the caller's outlet scope, newest first.
type ExceptionQuery struct {
	Scope  auth.OutletScope
	Status string
	After  ids.ID
	Limit  int
}

// Tx is one tenant-scoped transaction.
type Tx interface {
	// ---- outlets (projected from tenancy events; outlet-scoped) ----
	UpsertOutlet(ctx context.Context, o model.OutletRef) error
	GetOutlet(ctx context.Context, id ids.ID) (model.OutletRef, error)
	ListOutlets(ctx context.Context, scope auth.OutletScope) ([]model.OutletRef, error)
	// BrandsOf returns the distinct brands of the outlets inside scope.
	BrandsOf(ctx context.Context, scope auth.OutletScope) ([]ids.ID, error)

	// ---- projection of synced ops ----
	// LockProjection serialises projection writers of the tenant.
	LockProjection(ctx context.Context) error
	// OpState reports whether the op was seen and whether it is voided. A void may arrive before its op.
	OpState(ctx context.Context, key string) (seen, voided bool, err error)
	PutOp(ctx context.Context, key, stream, intent string, voided bool, now time.Time) error
	// VoidOp marks the op voided (creating the marker if the op was not seen), deletes its effects and
	// returns the rows they touched.
	VoidOp(ctx context.Context, key string, now time.Time) ([]RowKey, error)
	AddEffects(ctx context.Context, fx []EffectRec) error
	EffectsOf(ctx context.Context, k RowKey) ([]proj.Effect, error)
	PutRow(ctx context.Context, r model.Row) error
	// DeleteRow removes a projected row whose last insert was voided.
	DeleteRow(ctx context.Context, k RowKey) error

	// ---- reads ----
	GetRow(ctx context.Context, q RowQuery, id string) (model.Row, error)
	ListRows(ctx context.Context, q RowQuery) ([]model.Row, error)

	// ---- exceptions (outlet-scoped) ----
	// UpsertException inserts a finding or updates the status of the one with the same dedupe key.
	UpsertException(ctx context.Context, e model.Exception) error
	ListExceptions(ctx context.Context, q ExceptionQuery) ([]model.Exception, error)
	// SupersedeExceptions marks the open findings about a subject that are not in keep (dedupe keys) superseded.
	SupersedeExceptions(ctx context.Context, subjectKind, subjectID string, keep []string, now time.Time) error

	// ---- menu import jobs ----
	InsertImport(ctx context.Context, im model.Import) error
	GetImport(ctx context.Context, id ids.ID) (model.Import, error)
	UpdateImport(ctx context.Context, im model.Import) error
	ListImports(ctx context.Context, after ids.ID, limit int) ([]model.Import, error)

	// ---- audit (who, what, when, before and after of the strong-session actions) ----
	AppendAudit(ctx context.Context, a model.AuditEntry) error

	// ---- idempotency (scoped by tenant, operation and key) ----
	IdemGet(ctx context.Context, op, key string) (fingerprint string, response []byte, found bool, err error)
	IdemPut(ctx context.Context, op, key, fingerprint string, response []byte, now time.Time) error

	// ---- events ----
	// Publish writes e to the outbox in this transaction, assigning key_seq per key.
	Publish(ctx context.Context, e events.Envelope) error
}
