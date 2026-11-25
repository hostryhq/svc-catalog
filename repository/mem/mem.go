// Package mem is the in-memory implementation of repository.Repository. The test double runs the real
// service and API over it. Transactions are serialised and copy-on-write, so a failed transaction (or a failed
// vendor audit write) leaves no trace, as in Postgres. The query semantics (filters, ordering, keyset
// pagination, outlet scope) mirror the SQL of repository/pg; the shared suites run against both.
package mem

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/auditsink"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/nulldb"
	"github.com/hostryhq/svc-catalog/repository"
)

var errAuditDown = errors.New("mem: vendor access log unwritable")

type rowKey struct{ entity, id string }
type idemKey struct{ op, key string }
type idemVal struct {
	fp   string
	resp []byte
}

// queued is one row of the in-memory emit_queue.
type queued struct {
	op   emit.Op
	sent bool
}

// QueuedEmit is a queued op that has not been sent.
type QueuedEmit struct {
	Tenant ids.ID
	Op     emit.Op
}

// VendorLogEntry is one vendor_access_log row.
type VendorLogEntry struct{ auth.VendorAccessEntry }

// tenantState is everything one tenant owns.
type tenantState struct {
	outlets map[ids.ID]model.OutletRef
	cands   map[gsync.RowKey][]gsync.Candidate // append-only per row; replaced, never mutated in place
	voids   map[gsync.OpRef]bool
	queue   []queued
	rows    map[rowKey]model.Row
	excs    map[string]model.Exception // by dedupe key
	imports map[ids.ID]model.Import
	idem    map[idemKey]idemVal
	keySeq  map[string]int64
	outbox  []events.Envelope
	vlog    []VendorLogEntry
	audit   []model.AuditEntry
}

func newTenant() *tenantState {
	return &tenantState{outlets: map[ids.ID]model.OutletRef{}, cands: map[gsync.RowKey][]gsync.Candidate{}, voids: map[gsync.OpRef]bool{},
		rows: map[rowKey]model.Row{}, excs: map[string]model.Exception{}, imports: map[ids.ID]model.Import{},
		idem: map[idemKey]idemVal{}, keySeq: map[string]int64{}}
}

func cloneRow(r model.Row) model.Row {
	f := make(map[string]any, len(r.Fields))
	for k, v := range r.Fields {
		f[k] = v
	}
	s := make(map[string]string, len(r.Stamps))
	for k, v := range r.Stamps {
		s[k] = v
	}
	r.Fields, r.Stamps = f, s
	return r
}

func (t *tenantState) clone() *tenantState {
	c := newTenant()
	for k, v := range t.outlets {
		c.outlets[k] = v
	}
	for k, v := range t.cands {
		c.cands[k] = v // slices are append-only per key; replaced, never mutated in place
	}
	for k, v := range t.voids {
		c.voids[k] = v
	}
	c.queue = append([]queued(nil), t.queue...)
	for k, v := range t.rows {
		c.rows[k] = v // rows are replaced, never mutated in place
	}
	for k, v := range t.excs {
		c.excs[k] = v
	}
	for k, v := range t.imports {
		c.imports[k] = v
	}
	for k, v := range t.idem {
		c.idem[k] = v
	}
	for k, v := range t.keySeq {
		c.keySeq[k] = v
	}
	c.outbox = append([]events.Envelope(nil), t.outbox...)
	c.vlog = append([]VendorLogEntry(nil), t.vlog...)
	c.audit = append([]model.AuditEntry(nil), t.audit...)
	return c
}

// Store is the in-memory repository. It also implements auth.VendorAuditSink: the vendor access log row of a
// support-token request is written inside the running transaction.
type Store struct {
	mu        sync.Mutex
	tenants   map[ids.ID]*tenantState
	pending   *tenantState
	pendingID ids.ID
	failAudit bool
	db        *sql.DB
}

// New returns an empty store.
func New() *Store { return &Store{tenants: map[ids.ID]*tenantState{}, db: nulldb.Open()} }

var (
	_ repository.Repository = (*Store)(nil)
	_ auth.VendorAuditSink  = (*Store)(nil)
)

func (s *Store) tenant(t ids.ID) *tenantState {
	if ts, ok := s.tenants[t]; ok {
		return ts
	}
	ts := newTenant()
	s.tenants[t] = ts
	return ts
}

// Events returns the events written to the outbox of a tenant, in order.
func (s *Store) Events(tenant ids.ID) []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Envelope(nil), s.tenant(tenant).outbox...)
}

// VendorLog returns the vendor_access_log rows of a tenant, in order.
func (s *Store) VendorLog(tenant ids.ID) []VendorLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]VendorLogEntry(nil), s.tenant(tenant).vlog...)
}

// Reset drops every row of every tenant (used to load an example world).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants = map[ids.ID]*tenantState{}
}

// AuditLog returns the audit entries of a tenant, in order.
func (s *Store) AuditLog(tenant ids.ID) []model.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.AuditEntry(nil), s.tenant(tenant).audit...)
}

// FailAuditWrites makes the vendor access log unwritable, to prove that support requests fail closed.
func (s *Store) FailAuditWrites(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAudit = fail
}

// RecordInTx writes the log row into the running transaction.
func (s *Store) RecordInTx(_ context.Context, _ tenancy.Execer, e auth.VendorAccessEntry) error {
	if s.failAudit {
		return errAuditDown
	}
	s.pending.vlog = append(s.pending.vlog, VendorLogEntry{keyed(e)})
	return nil
}

// RecordRefused records a refused request in a transaction of its own.
func (s *Store) RecordRefused(_ context.Context, e auth.VendorAccessEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		return nil // no organisation to attach the row to
	}
	e.Outcome = auth.OutcomeRefused
	ts := s.tenant(tid)
	ts.vlog = append(ts.vlog, VendorLogEntry{keyed(e)})
	return nil
}

func keyed(e auth.VendorAccessEntry) auth.VendorAccessEntry {
	e.ActionKey = auditsink.ActionKey(e.ActionKey)
	return e
}

// Ping implements repository.Repository.
func (s *Store) Ping(context.Context) error { return nil }

// InTx implements repository.Repository.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.tenant(tc.TenantID).clone()
	s.pending, s.pendingID = work, tc.TenantID
	defer func() { s.pending = nil }()
	if err := auth.WithAuditedTx(ctx, s.db, func(c context.Context, _ *sql.Tx) error {
		return fn(c, &tx{st: work})
	}); err != nil {
		return err
	}
	s.tenants[tc.TenantID] = work
	return nil
}

type tx struct{ st *tenantState }

func sortKey(r model.Row) int64 { return r.Int("sort_order") }

func less(a, b model.Row) bool {
	if sortKey(a) != sortKey(b) {
		return sortKey(a) < sortKey(b)
	}
	return a.ID < b.ID
}

// ---- outlets ----

func (t *tx) UpsertOutlet(_ context.Context, o model.OutletRef) error {
	t.st.outlets[o.ID] = o
	return nil
}

func (t *tx) GetOutlet(_ context.Context, id ids.ID) (model.OutletRef, error) {
	o, ok := t.st.outlets[id]
	if !ok {
		return model.OutletRef{}, model.ErrRepoNotFound
	}
	return o, nil
}

func (t *tx) ListOutlets(_ context.Context, scope auth.OutletScope) ([]model.OutletRef, error) {
	var out []model.OutletRef
	for _, o := range t.st.outlets {
		if scope.Contains(o.ID.String()) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *tx) BrandsOf(ctx context.Context, scope auth.OutletScope) ([]ids.ID, error) {
	os, _ := t.ListOutlets(ctx, scope)
	seen := map[ids.ID]bool{}
	var out []ids.ID
	for _, o := range os {
		if !seen[o.BrandID] {
			seen[o.BrandID] = true
			out = append(out, o.BrandID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ---- projection ----

func (t *tx) LockProjection(context.Context) error { return nil } // transactions are serialised

// SyncStore implements repository.Tx: the candidates and void set of the tenant, inside the transaction.
func (t *tx) SyncStore() gsync.Store { return memSync{t.st} }

type memSync struct{ st *tenantState }

func (m memSync) AddCandidate(_ context.Context, k gsync.RowKey, c gsync.Candidate) error {
	cur := m.st.cands[k]
	for _, e := range cur {
		if e.Ref == c.Ref && e.Idx == c.Idx {
			return nil // idempotent: the same (op, mutation index) is recorded once
		}
	}
	m.st.cands[k] = append(append([]gsync.Candidate(nil), cur...), c)
	return nil
}

func (m memSync) Candidates(_ context.Context, k gsync.RowKey) ([]gsync.Candidate, error) {
	return append([]gsync.Candidate(nil), m.st.cands[k]...), nil
}

func (m memSync) RowsOfOp(_ context.Context, _ string, r gsync.OpRef) ([]gsync.RowKey, error) {
	var out []gsync.RowKey
	for k, cs := range m.st.cands {
		for _, c := range cs {
			if c.Ref == r {
				out = append(out, k)
				break
			}
		}
	}
	return out, nil
}

func (m memSync) AddVoid(_ context.Context, _ string, r gsync.OpRef) error {
	m.st.voids[r] = true
	return nil
}

func (m memSync) Voided(_ context.Context, _ string, refs []gsync.OpRef) (map[gsync.OpRef]bool, error) {
	out := map[gsync.OpRef]bool{}
	for _, r := range refs {
		if m.st.voids[r] {
			out[r] = true
		}
	}
	return out, nil
}

// EmitEnqueue implements repository.Tx: an op whose key is already queued is ignored.
func (t *tx) EmitEnqueue(_ context.Context, ops ...emit.Op) error {
	for _, o := range ops {
		if err := o.Validate(); err != nil {
			return err
		}
		dup := false
		for _, q := range t.st.queue {
			dup = dup || q.op.Key == o.Key
		}
		if !dup {
			t.st.queue = append(t.st.queue, queued{op: o})
		}
	}
	return nil
}

// PendingEmits returns the queued ops that were not sent, per tenant in enqueue order (the double's worker).
func (s *Store) PendingEmits() []QueuedEmit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []QueuedEmit
	for tid, ts := range s.tenants {
		for _, q := range ts.queue {
			if !q.sent {
				out = append(out, QueuedEmit{Tenant: tid, Op: q.op})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

// MarkEmitted marks the queued op as sent.
func (s *Store) MarkEmitted(tenant ids.ID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tenant(tenant)
	q := append([]queued(nil), ts.queue...)
	for i := range q {
		if q[i].op.Key == key {
			q[i].sent = true
		}
	}
	ts.queue = q
}

func (t *tx) PutRow(_ context.Context, r model.Row) error {
	t.st.rows[rowKey{r.Entity, r.ID}] = cloneRow(r)
	return nil
}

func (t *tx) DeleteRow(_ context.Context, k repository.RowKey) error {
	delete(t.st.rows, rowKey{k.Entity, k.ID})
	return nil
}

// ---- reads ----

func (t *tx) matches(q repository.RowQuery, r model.Row) bool {
	if r.Entity != q.Entity || r.Gone != q.Gone {
		return false
	}
	switch r.Stream {
	case model.StreamBrand:
		if !q.Brands.All && !containsID(q.Brands.IDs, r.ScopeID) {
			return false
		}
	default:
		if !q.Scope.Contains(r.ScopeID.String()) {
			return false
		}
	}
	for f, v := range q.Eq {
		if r.Str(f) != v {
			return false
		}
	}
	for f, vs := range q.In {
		ok := false
		for _, v := range vs {
			ok = ok || r.Str(f) == v
		}
		if !ok {
			return false
		}
	}
	for f, want := range q.BoolEq {
		got := true
		if r.Has(f) {
			got = r.Bool(f)
		}
		if got != want {
			return false
		}
	}
	if q.Search != "" {
		hit := q.ExactField != "" && r.Str(q.ExactField) == q.Search
		needle := strings.ToLower(q.Search)
		for _, f := range q.SearchFields {
			hit = hit || strings.Contains(strings.ToLower(r.Str(f)), needle)
		}
		if !hit {
			return false
		}
	}
	return true
}

func containsID(xs []ids.ID, x ids.ID) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (t *tx) GetRow(_ context.Context, q repository.RowQuery, id string) (model.Row, error) {
	r, ok := t.st.rows[rowKey{q.Entity, id}]
	if !ok {
		return model.Row{}, model.ErrRepoNotFound
	}
	// A direct read ignores the live/tombstone selector: an archived item is still readable by id.
	g := q
	g.Gone = r.Gone
	g.Eq, g.In, g.Search = nil, nil, ""
	if !t.matches(g, r) {
		return model.Row{}, model.ErrRepoNotFound
	}
	return cloneRow(r), nil
}

func (t *tx) ListRows(_ context.Context, q repository.RowQuery) ([]model.Row, error) {
	var all []model.Row
	for _, r := range t.st.rows {
		if t.matches(q, r) {
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	if q.After != "" {
		if a, ok := t.st.rows[rowKey{q.Entity, q.After}]; ok {
			i := sort.Search(len(all), func(i int) bool { return less(a, all[i]) })
			all = all[i:]
		}
	}
	if q.Limit > 0 && len(all) > q.Limit {
		all = all[:q.Limit]
	}
	out := make([]model.Row, len(all))
	for i, r := range all {
		out[i] = cloneRow(r)
	}
	return out, nil
}

// ---- exceptions ----

func (t *tx) UpsertException(_ context.Context, e model.Exception) error {
	if old, ok := t.st.excs[e.DedupeKey]; ok {
		old.Status, old.ResolvedAt, old.ResolvedRef, old.Params = e.Status, e.ResolvedAt, e.ResolvedRef, e.Params
		t.st.excs[e.DedupeKey] = old
		return nil
	}
	t.st.excs[e.DedupeKey] = e
	return nil
}

func (t *tx) ListExceptions(_ context.Context, q repository.ExceptionQuery) ([]model.Exception, error) {
	var all []model.Exception
	for _, e := range t.st.excs {
		if !q.Scope.Contains(e.OutletID.String()) || (q.Status != "" && e.Status != q.Status) {
			continue
		}
		all = append(all, e)
	}
	newer := func(a, b model.Exception) bool { // newest first
		if !a.DetectedAt.Equal(b.DetectedAt) {
			return a.DetectedAt.After(b.DetectedAt)
		}
		return a.ID > b.ID
	}
	sort.Slice(all, func(i, j int) bool { return newer(all[i], all[j]) })
	if q.After != "" {
		for _, a := range t.st.excs {
			if a.ID == q.After {
				i := sort.Search(len(all), func(i int) bool { return newer(a, all[i]) })
				all = all[i:]
				break
			}
		}
	}
	if q.Limit > 0 && len(all) > q.Limit {
		all = all[:q.Limit]
	}
	return all, nil
}

func (t *tx) SupersedeExceptions(_ context.Context, kind, subject string, keep []string, now time.Time) error {
	for k, e := range t.st.excs {
		if e.SubjectKind != kind || e.SubjectID != subject || e.Status != model.StatusOpen {
			continue
		}
		kept := false
		for _, x := range keep {
			kept = kept || x == k
		}
		if !kept {
			e.Status = model.StatusSuperseded
			e.ResolvedAt = &now
			t.st.excs[k] = e
		}
	}
	return nil
}

func (t *tx) AppendAudit(_ context.Context, a model.AuditEntry) error {
	t.st.audit = append(t.st.audit, a)
	return nil
}

// ---- imports ----

func (t *tx) InsertImport(_ context.Context, im model.Import) error {
	t.st.imports[im.ID] = im
	return nil
}

func (t *tx) GetImport(_ context.Context, id ids.ID) (model.Import, error) {
	im, ok := t.st.imports[id]
	if !ok {
		return model.Import{}, model.ErrRepoNotFound
	}
	return im, nil
}

func (t *tx) UpdateImport(_ context.Context, im model.Import) error {
	if _, ok := t.st.imports[im.ID]; !ok {
		return model.ErrRepoNotFound
	}
	t.st.imports[im.ID] = im
	return nil
}

func (t *tx) ListImports(_ context.Context, after ids.ID, limit int) ([]model.Import, error) {
	var all []model.Import
	for _, im := range t.st.imports {
		all = append(all, im)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID }) // newest first (UUIDv7)
	if _, ok := t.st.imports[after]; ok && after != "" {
		i := sort.Search(len(all), func(i int) bool { return all[i].ID < after })
		all = all[i:]
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ---- idempotency, events ----

func (t *tx) IdemGet(_ context.Context, op, key string) (string, []byte, bool, error) {
	v, ok := t.st.idem[idemKey{op, key}]
	return v.fp, v.resp, ok, nil
}

func (t *tx) IdemPut(_ context.Context, op, key, fp string, resp []byte, _ time.Time) error {
	t.st.idem[idemKey{op, key}] = idemVal{fp, resp}
	return nil
}

func (t *tx) Publish(_ context.Context, e events.Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	t.st.keySeq[e.Key]++
	e.KeySeq = t.st.keySeq[e.Key]
	t.st.outbox = append(t.st.outbox, e)
	return nil
}
