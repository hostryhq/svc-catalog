// Package pg is the Postgres implementation of repository.Repository. Every transaction is opened through
// go-auth's audited transaction (which uses go-common's tenancy helper), so the tenant context is applied to
// the transaction, and only to it, before any statement, and a request made on a support session's authority
// writes its vendor_access_log row in the same transaction. Every statement also carries an explicit tenant
// predicate; statements on outlet-scoped tables carry a bound outlet_id predicate (scopelint). SQL is
// parameterised; nothing is built from request data.
package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/outbox"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/repository"
)

// Repo is the Postgres repository; app runs as catalog_app.
type Repo struct{ app *sql.DB }

// New wraps the application pool, which must have passed VerifyRuntimeRole.
func New(app *sql.DB) *Repo { return &Repo{app: app} }

// Open opens a pgx-backed pool with conservative limits. Every connection is reset (RESET ALL) when it goes
// back to the pool, so a session-level setting such as app.tenant_id can never leak to the next request
// (ADR-0003 T4); the tenant context is only ever set per transaction.
func Open(dsn string, maxConns int) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg, stdlib.OptionResetSession(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "RESET ALL")
		return err
	}))
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

var _ repository.Repository = (*Repo)(nil)

// InTx implements repository.Repository.
func (r *Repo) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	return auth.WithAuditedTx(ctx, r.app, func(ctx context.Context, tx *sql.Tx) error {
		return fn(ctx, &pgTx{tx: tx, t: tc.TenantID})
	})
}

// Bind returns the repository transaction for an already open database transaction, after applying the
// tenant context to it. The bus consumer uses it so the projection commits with the inbox row (ADR-0011 B5).
func Bind(ctx context.Context, tx *sql.Tx, tenant ids.ID) (repository.Tx, error) {
	if err := tenancy.SetDBContext(ctx, tx, tenant); err != nil {
		return nil, err
	}
	return &pgTx{tx: tx, t: tenant}, nil
}

// Ping implements repository.Repository.
func (r *Repo) Ping(ctx context.Context) error { return r.app.PingContext(ctx) }

// VerifyRuntimeRole refuses a database role that could defeat row-level security (TC-01): superuser,
// BYPASSRLS, or the owner of any table. It is run for every runtime pool at start-up.
func VerifyRuntimeRole(ctx context.Context, db *sql.DB) error {
	var name string
	var super, bypass bool
	if err := db.QueryRowContext(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&name, &super, &bypass); err != nil {
		return err
	}
	if super || bypass {
		return errors.New("pg: runtime role " + name + " is superuser or BYPASSRLS; refusing to start")
	}
	var owned int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return errors.New("pg: runtime role " + name + " owns tables (the migrator role does); refusing to start")
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrRepoNotFound
	}
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// decodeMap decodes a JSON object; integers come back as int64 (op values are integers only).
func decodeMap(b []byte) map[string]any {
	out := map[string]any{}
	if len(b) == 0 {
		return out
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	_ = dec.Decode(&out)
	for k, v := range out {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				out[k] = i
			}
		}
	}
	return out
}

func decodeStrings(b []byte) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(b, &out)
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func idStrings(in []ids.ID) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.String()
	}
	return out
}

func scopeIDs(s auth.OutletScope) []string {
	if s.All() {
		return []string{}
	}
	return s.IDs()
}

// pgTx is one tenant-scoped transaction.
type pgTx struct {
	tx *sql.Tx
	t  ids.ID
}

// ---- outlets ----

func (x *pgTx) UpsertOutlet(ctx context.Context, o model.OutletRef) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO outlets (tenant_id, outlet_id, brand_id, kind, status) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, outlet_id) DO UPDATE SET brand_id = $3, kind = $4, status = $5`, x.t, o.ID, o.BrandID, o.Kind, o.Status)
	return err
}

func (x *pgTx) GetOutlet(ctx context.Context, id ids.ID) (model.OutletRef, error) {
	o := model.OutletRef{ID: id}
	err := x.tx.QueryRowContext(ctx, `SELECT brand_id, kind, status FROM outlets WHERE tenant_id = $1 AND outlet_id = $2`, x.t, id).Scan(&o.BrandID, &o.Kind, &o.Status)
	return o, notFound(err)
}

func (x *pgTx) ListOutlets(ctx context.Context, scope auth.OutletScope) ([]model.OutletRef, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT outlet_id, brand_id, kind, status FROM outlets
		WHERE tenant_id = $1 AND ($2::boolean OR outlet_id = ANY($3::uuid[])) ORDER BY outlet_id`, x.t, scope.All(), scopeIDs(scope))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.OutletRef
	for rows.Next() {
		var o model.OutletRef
		if err := rows.Scan(&o.ID, &o.BrandID, &o.Kind, &o.Status); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (x *pgTx) BrandsOf(ctx context.Context, scope auth.OutletScope) ([]ids.ID, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT DISTINCT brand_id FROM outlets
		WHERE tenant_id = $1 AND ($2::boolean OR outlet_id = ANY($3::uuid[])) ORDER BY brand_id`, x.t, scope.All(), scopeIDs(scope))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ids.ID
	for rows.Next() {
		var b ids.ID
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- projection ----

func (x *pgTx) LockProjection(ctx context.Context) error {
	_, err := x.tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7464012001))`, x.t.String())
	return err
}

func (x *pgTx) OpState(ctx context.Context, key string) (bool, bool, error) {
	var voided bool
	err := x.tx.QueryRowContext(ctx, `SELECT voided FROM ops WHERE tenant_id = $1 AND op_key = $2`, x.t, key).Scan(&voided)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, voided, err
}

func (x *pgTx) PutOp(ctx context.Context, key, stream, intent string, voided bool, now time.Time) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO ops (tenant_id, op_key, stream, intent, voided, applied_at) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant_id, op_key) DO NOTHING`, x.t, key, stream, intent, voided, now.UTC())
	return err
}

func (x *pgTx) VoidOp(ctx context.Context, key string, now time.Time) ([]repository.RowKey, error) {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO ops (tenant_id, op_key, stream, intent, voided, applied_at) VALUES ($1,$2,'','',true,$3)
		ON CONFLICT (tenant_id, op_key) DO UPDATE SET voided = true`, x.t, key, now.UTC()); err != nil {
		return nil, err
	}
	rows, err := x.tx.QueryContext(ctx, `DELETE FROM effects WHERE tenant_id = $1 AND op_key = $2 RETURNING entity, row_id, stream, scope_id`, x.t, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []repository.RowKey
	for rows.Next() {
		var k repository.RowKey
		if err := rows.Scan(&k.Entity, &k.ID, &k.Stream, &k.ScopeID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (x *pgTx) AddEffects(ctx context.Context, fx []repository.EffectRec) error {
	for _, e := range fx {
		prev := e.Prev
		if prev == nil {
			prev = map[string]string{}
		}
		if _, err := x.tx.ExecContext(ctx, `INSERT INTO effects (tenant_id, entity, row_id, op_key, idx, stream, scope_id, hlc, origin, seq, kind, fields, prev)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`,
			x.t, e.Entity, e.RowID, e.Op, e.St.Idx, e.Stream, e.ScopeID, int64(e.St.HLC), e.St.Origin, e.St.Seq, e.Kind, mustJSON(e.F), mustJSON(prev)); err != nil {
			return err
		}
	}
	return nil
}

func (x *pgTx) EffectsOf(ctx context.Context, k repository.RowKey) ([]proj.Effect, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT op_key, idx, hlc, origin, seq, kind, fields, prev FROM effects
		WHERE tenant_id = $1 AND entity = $2 AND row_id = $3`, x.t, k.Entity, k.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []proj.Effect
	for rows.Next() {
		var e proj.Effect
		var hlc int64
		var f, p []byte
		if err := rows.Scan(&e.Op, &e.St.Idx, &hlc, &e.St.Origin, &e.St.Seq, &e.Kind, &f, &p); err != nil {
			return nil, err
		}
		e.St.HLC, e.F, e.Prev = uint64(hlc), decodeMap(f), decodeStrings(p)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (x *pgTx) PutRow(ctx context.Context, r model.Row) error {
	if r.Stream == model.StreamOutlet {
		_, err := x.tx.ExecContext(ctx, `INSERT INTO outlet_rows (tenant_id, outlet_id, entity, row_id, sort_key, gone, fields, stamps, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, entity, row_id) DO UPDATE SET outlet_id = $2, sort_key = $5, gone = $6, fields = $7, stamps = $8, updated_at = $9`,
			x.t, r.ScopeID, r.Entity, r.ID, r.Int("sort_order"), r.Gone, mustJSON(r.Fields), mustJSON(r.Stamps), r.UpdatedAt.UTC())
		return err
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO brand_rows (tenant_id, brand_id, entity, row_id, sort_key, gone, fields, stamps, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, entity, row_id) DO UPDATE SET brand_id = $2, sort_key = $5, gone = $6, fields = $7, stamps = $8, updated_at = $9`,
		x.t, r.ScopeID, r.Entity, r.ID, r.Int("sort_order"), r.Gone, mustJSON(r.Fields), mustJSON(r.Stamps), r.UpdatedAt.UTC())
	return err
}

func (x *pgTx) DeleteRow(ctx context.Context, k repository.RowKey) error {
	if k.Stream == model.StreamOutlet {
		_, err := x.tx.ExecContext(ctx, `DELETE FROM outlet_rows WHERE tenant_id = $1 AND outlet_id = $2 AND entity = $3 AND row_id = $4`, x.t, k.ScopeID, k.Entity, k.ID)
		return err
	}
	_, err := x.tx.ExecContext(ctx, `DELETE FROM brand_rows WHERE tenant_id = $1 AND entity = $2 AND row_id = $3`, x.t, k.Entity, k.ID)
	return err
}

// ---- reads ----

type rowScanner interface{ Scan(dest ...any) error }

func scanRow(s rowScanner, entity, stream string) (model.Row, error) {
	r := model.Row{Entity: entity, Stream: stream}
	var f, st []byte
	if err := s.Scan(&r.ID, &r.ScopeID, &r.Gone, &f, &st, &r.UpdatedAt); err != nil {
		return r, err
	}
	r.Fields, r.Stamps, r.UpdatedAt = decodeMap(f), decodeStrings(st), r.UpdatedAt.UTC()
	return r, nil
}

func streamOf(entity string) string {
	if d, ok := proj.Entities[entity]; ok {
		return d.Stream
	}
	return model.StreamBrand
}

func (x *pgTx) GetRow(ctx context.Context, q repository.RowQuery, id string) (model.Row, error) {
	if streamOf(q.Entity) == model.StreamOutlet {
		r, err := scanRow(x.tx.QueryRowContext(ctx, `SELECT row_id, outlet_id, gone, fields, stamps, updated_at FROM outlet_rows
			WHERE tenant_id = $1 AND entity = $2 AND row_id = $3 AND ($4::boolean OR outlet_id = ANY($5::uuid[]))`,
			x.t, q.Entity, id, q.Scope.All(), scopeIDs(q.Scope)), q.Entity, model.StreamOutlet)
		return r, notFound(err)
	}
	r, err := scanRow(x.tx.QueryRowContext(ctx, `SELECT row_id, brand_id, gone, fields, stamps, updated_at FROM brand_rows
		WHERE tenant_id = $1 AND entity = $2 AND row_id = $3 AND ($4::boolean OR brand_id = ANY($5::uuid[]))`,
		x.t, q.Entity, id, q.Brands.All, idStrings(q.Brands.IDs)), q.Entity, model.StreamBrand)
	return r, notFound(err)
}

// likePattern escapes LIKE wildcards of a user's search text.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.ToLower(s)) + "%"
}

// afterKey returns the sort key of the row a page continues after; ok is false when it does not exist (the
// page then starts from the beginning).
func (x *pgTx) afterKey(ctx context.Context, q repository.RowQuery) (int64, bool, error) {
	if q.After == "" {
		return 0, false, nil
	}
	var sk int64
	var err error
	if streamOf(q.Entity) == model.StreamOutlet {
		err = x.tx.QueryRowContext(ctx, `SELECT sort_key FROM outlet_rows WHERE tenant_id = $1 AND entity = $2 AND row_id = $3 AND ($4::boolean OR outlet_id = ANY($5::uuid[]))`,
			x.t, q.Entity, q.After, q.Scope.All(), scopeIDs(q.Scope)).Scan(&sk)
	} else {
		err = x.tx.QueryRowContext(ctx, `SELECT sort_key FROM brand_rows WHERE tenant_id = $1 AND entity = $2 AND row_id = $3`, x.t, q.Entity, q.After).Scan(&sk)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return sk, err == nil, err
}

func (x *pgTx) ListRows(ctx context.Context, q repository.RowQuery) ([]model.Row, error) {
	sk, hasAfter, err := x.afterKey(ctx, q)
	if err != nil {
		return nil, err
	}
	eq, _ := json.Marshal(q.Eq)
	if len(q.Eq) == 0 {
		eq = []byte("{}")
	}
	var inField string
	var inVals []string
	for f, vs := range q.In {
		inField, inVals = f, vs
	}
	var boolField string
	var boolVal bool
	for f, v := range q.BoolEq {
		boolField, boolVal = f, v
	}
	search, exact := "", ""
	if q.Search != "" {
		search, exact = likePattern(q.Search), q.Search
	}
	after := ""
	if hasAfter {
		after = q.After
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1_000_000
	}
	var rows *sql.Rows
	if streamOf(q.Entity) == model.StreamOutlet {
		rows, err = x.tx.QueryContext(ctx, `SELECT row_id, outlet_id, gone, fields, stamps, updated_at FROM outlet_rows
			WHERE tenant_id = $1 AND entity = $2 AND gone = $3 AND ($4::boolean OR outlet_id = ANY($5::uuid[]))
			  AND fields @> $6::jsonb
			  AND ($7::text = '' OR fields ->> $7 = ANY($8::text[]))
			  AND ($9::text = '' OR COALESCE((fields ->> $9)::boolean, true) = $10)
			  AND ($11::text = '' OR fields ->> $12 = $13 OR EXISTS (SELECT 1 FROM unnest($14::text[]) f WHERE lower(fields ->> f) LIKE $11 ESCAPE '\'))
			  AND ($15::text = '' OR (sort_key, row_id COLLATE "C") > ($16::bigint, $15::text COLLATE "C"))
			ORDER BY sort_key, row_id COLLATE "C" LIMIT $17`,
			x.t, q.Entity, q.Gone, q.Scope.All(), scopeIDs(q.Scope), string(eq), inField, inVals, boolField, boolVal, search, q.ExactField, exact, q.SearchFields, after, sk, limit)
	} else {
		rows, err = x.tx.QueryContext(ctx, `SELECT row_id, brand_id, gone, fields, stamps, updated_at FROM brand_rows
			WHERE tenant_id = $1 AND entity = $2 AND gone = $3 AND ($4::boolean OR brand_id = ANY($5::uuid[]))
			  AND fields @> $6::jsonb
			  AND ($7::text = '' OR fields ->> $7 = ANY($8::text[]))
			  AND ($9::text = '' OR COALESCE((fields ->> $9)::boolean, true) = $10)
			  AND ($11::text = '' OR fields ->> $12 = $13 OR EXISTS (SELECT 1 FROM unnest($14::text[]) f WHERE lower(fields ->> f) LIKE $11 ESCAPE '\'))
			  AND ($15::text = '' OR (sort_key, row_id COLLATE "C") > ($16::bigint, $15::text COLLATE "C"))
			ORDER BY sort_key, row_id COLLATE "C" LIMIT $17`,
			x.t, q.Entity, q.Gone, q.Brands.All, idStrings(q.Brands.IDs), string(eq), inField, inVals, boolField, boolVal, search, q.ExactField, exact, q.SearchFields, after, sk, limit)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Row
	for rows.Next() {
		r, err := scanRow(rows, q.Entity, streamOf(q.Entity))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- exceptions ----

func (x *pgTx) UpsertException(ctx context.Context, e model.Exception) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO exceptions (tenant_id, id, outlet_id, dedupe_key, kind, severity, status, subject_kind, subject_id, detected_at, resolved_at, resolved_ref, detail_key, params)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (tenant_id, dedupe_key) DO UPDATE SET status = $7, resolved_at = $11, resolved_ref = $12, params = $14`,
		x.t, e.ID, e.OutletID, e.DedupeKey, e.Kind, e.Severity, e.Status, e.SubjectKind, e.SubjectID, e.DetectedAt.UTC(), e.ResolvedAt, e.ResolvedRef, e.DetailKey, mustJSON(e.Params))
	return err
}

func (x *pgTx) ListExceptions(ctx context.Context, q repository.ExceptionQuery) ([]model.Exception, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 1_000_000
	}
	// A cursor names the last row of the previous page; it must itself be in the caller's scope, and an unknown
	// one starts from the beginning.
	var afterAt time.Time
	var afterID string
	hasAfter := false
	if q.After != "" {
		err := x.tx.QueryRowContext(ctx, `SELECT detected_at, id FROM exceptions WHERE tenant_id = $1 AND id = $2::uuid AND ($3::boolean OR outlet_id = ANY($4::uuid[]))`,
			x.t, q.After.String(), q.Scope.All(), scopeIDs(q.Scope)).Scan(&afterAt, &afterID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		hasAfter = err == nil
	}
	rows, err := x.tx.QueryContext(ctx, `SELECT id, outlet_id, kind, severity, status, subject_kind, subject_id, detected_at, resolved_at, resolved_ref, detail_key, params, dedupe_key
		FROM exceptions
		WHERE tenant_id = $1 AND ($2::boolean OR outlet_id = ANY($3::uuid[])) AND ($4::text = '' OR status = $4)
		  AND (NOT $5::boolean OR (detected_at, id) < ($6::timestamptz, $7::uuid))
		ORDER BY detected_at DESC, id DESC LIMIT $8`, x.t, q.Scope.All(), scopeIDs(q.Scope), q.Status, hasAfter, afterAt, nilUUID(afterID), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Exception
	for rows.Next() {
		var e model.Exception
		var p []byte
		if err := rows.Scan(&e.ID, &e.OutletID, &e.Kind, &e.Severity, &e.Status, &e.SubjectKind, &e.SubjectID, &e.DetectedAt, &e.ResolvedAt, &e.ResolvedRef, &e.DetailKey, &p, &e.DedupeKey); err != nil {
			return nil, err
		}
		e.Params, e.DetectedAt = decodeMap(p), e.DetectedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func nilUUID(s string) string {
	if s == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return s
}

func (x *pgTx) SupersedeExceptions(ctx context.Context, kind, subject string, keep []string, now time.Time) error {
	if keep == nil {
		keep = []string{}
	}
	//scopelint:internal the subject row's findings are superseded across every outlet it concerned; the projection runs per tenant
	_, err := x.tx.ExecContext(ctx, `UPDATE exceptions SET status = 'superseded', resolved_at = $5
		WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3 AND status = 'open' AND NOT (dedupe_key = ANY($4::text[]))`, x.t, kind, subject, keep, now.UTC())
	return err
}

// ---- audit ----

func (x *pgTx) AppendAudit(ctx context.Context, a model.AuditEntry) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO audit_log (tenant_id, id, at, actor, action, subject_kind, subject_id, before, after, reason) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		x.t, a.ID, a.At.UTC(), a.Actor, a.Action, a.SubjectKind, a.SubjectID, a.Before, a.After, a.Reason)
	return err
}

// ---- imports ----

const importCols = `id, status, source, filename, row_count, error_count, errors, categories_created, items_created, items_updated, price_changes, created_at, committed_at, created_by, plan`

func scanImport(s rowScanner) (model.Import, error) {
	var im model.Import
	var errs, plan []byte
	if err := s.Scan(&im.ID, &im.Status, &im.Source, &im.Filename, &im.RowCount, &im.ErrorCount, &errs, &im.CategoriesCreated, &im.ItemsCreated,
		&im.ItemsUpdated, &im.PriceChanges, &im.CreatedAt, &im.CommittedAt, &im.CreatedBy, &plan); err != nil {
		return im, err
	}
	_ = json.Unmarshal(errs, &im.Errors)
	if im.Errors == nil {
		im.Errors = []model.ImportRowError{}
	}
	if len(plan) > 0 && string(plan) != "null" {
		im.Plan = &model.ImportPlan{}
		_ = json.Unmarshal(plan, im.Plan)
	}
	im.CreatedAt = im.CreatedAt.UTC()
	if im.CommittedAt != nil {
		t := im.CommittedAt.UTC()
		im.CommittedAt = &t
	}
	return im, nil
}

func planJSON(p *model.ImportPlan) any {
	if p == nil {
		return nil
	}
	return mustJSON(p)
}

func (x *pgTx) InsertImport(ctx context.Context, im model.Import) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO menu_imports (tenant_id, `+importCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		x.t, im.ID, im.Status, im.Source, im.Filename, im.RowCount, im.ErrorCount, mustJSON(im.Errors), im.CategoriesCreated, im.ItemsCreated, im.ItemsUpdated,
		im.PriceChanges, im.CreatedAt.UTC(), im.CommittedAt, im.CreatedBy, planJSON(im.Plan))
	return err
}

func (x *pgTx) GetImport(ctx context.Context, id ids.ID) (model.Import, error) {
	im, err := scanImport(x.tx.QueryRowContext(ctx, `SELECT `+importCols+` FROM menu_imports WHERE tenant_id = $1 AND id = $2`, x.t, id))
	return im, notFound(err)
}

func (x *pgTx) UpdateImport(ctx context.Context, im model.Import) error {
	res, err := x.tx.ExecContext(ctx, `UPDATE menu_imports SET status = $3, committed_at = $4, plan = $5 WHERE tenant_id = $1 AND id = $2`, x.t, im.ID, im.Status, im.CommittedAt, planJSON(im.Plan))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrRepoNotFound
	}
	return nil
}

func (x *pgTx) ListImports(ctx context.Context, after ids.ID, limit int) ([]model.Import, error) {
	if limit <= 0 {
		limit = 1_000_000
	}
	// Newest first (UUIDv7 ids sort by time); an unknown cursor starts from the beginning.
	hasAfter := false
	if after != "" {
		var one int
		err := x.tx.QueryRowContext(ctx, `SELECT 1 FROM menu_imports WHERE tenant_id = $1 AND id = $2::uuid`, x.t, after.String()).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		hasAfter = err == nil
	}
	rows, err := x.tx.QueryContext(ctx, `SELECT `+importCols+` FROM menu_imports
		WHERE tenant_id = $1 AND (NOT $2::boolean OR id < $3::uuid) ORDER BY id DESC LIMIT $4`, x.t, hasAfter, nilUUID(after.String()), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Import
	for rows.Next() {
		im, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// ---- idempotency, events ----

func (x *pgTx) IdemGet(ctx context.Context, op, key string) (string, []byte, bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id, op, key, fingerprint, response, created_at) VALUES ($1,$2,$3,'','',now()) ON CONFLICT DO NOTHING`, x.t, op, key)
	if err != nil {
		return "", nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return "", nil, false, nil
	}
	var fp string
	var resp []byte
	err = x.tx.QueryRowContext(ctx, `SELECT fingerprint, response FROM idempotency_keys WHERE tenant_id = $1 AND op = $2 AND key = $3`, x.t, op, key).Scan(&fp, &resp)
	return fp, resp, err == nil, err
}

func (x *pgTx) IdemPut(ctx context.Context, op, key, fp string, resp []byte, _ time.Time) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE idempotency_keys SET fingerprint = $4, response = $5 WHERE tenant_id = $1 AND op = $2 AND key = $3`, x.t, op, key, fp, resp)
	return err
}

func (x *pgTx) Publish(ctx context.Context, e events.Envelope) error {
	var seq int64
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO event_keys (tenant_id, key, last_seq) VALUES ($1,$2,1)
		ON CONFLICT (tenant_id, key) DO UPDATE SET last_seq = event_keys.last_seq + 1 RETURNING last_seq`, x.t, e.Key).Scan(&seq); err != nil {
		return err
	}
	e.KeySeq = seq
	return outbox.Publish(ctx, x.tx, e)
}
