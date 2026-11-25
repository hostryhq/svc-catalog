package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
)

// Event types catalog consumes besides the sync op events (sync.op.<namespace>, one durable consumer per
// namespace, ADR-0014 A2.1).
const (
	EventOutletCreated = "tenancy.outlet.created"
	EventOutletUpdated = "tenancy.outlet.updated"
)

// serviceName is catalog's name in the entity registry (owner, replicated_to).
const serviceName = "catalog"

// ApplyEvent applies one bus event inside tx (the tenant context of ctx is the event's tenant, and tx is bound
// to it). A sync op event is read only through go-common/sync.ParseOp (verification, size limits, upcasters);
// a malformed one is an error, so the inbox retries and then parks it. Applying is idempotent and needs no
// order (ADR-0014 A2.1 item 5).
func (s *Service) ApplyEvent(ctx context.Context, tx repository.Tx, env events.Envelope) error {
	switch {
	case strings.HasPrefix(env.Type, "sync.op."):
		op, err := s.parser.ParseEnvelope(env)
		if err != nil {
			return err
		}
		return s.ApplyOp(ctx, tx, op)
	case env.Type == EventOutletCreated || env.Type == EventOutletUpdated:
		var d struct {
			OutletID ids.ID `json:"outlet_id"`
			BrandID  ids.ID `json:"brand_id"`
			Kind     string `json:"kind"`
			Status   string `json:"status"`
		}
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return fmt.Errorf("service: outlet event data: %w", err)
		}
		if _, err := ids.Parse(d.OutletID.String()); err != nil {
			return fmt.Errorf("service: outlet event: outlet id: %w", err)
		}
		if _, err := ids.Parse(d.BrandID.String()); err != nil {
			return fmt.Errorf("service: outlet event: brand id: %w", err)
		}
		return tx.UpsertOutlet(ctx, model.OutletRef{ID: d.OutletID, BrandID: d.BrandID, Kind: d.Kind, Status: d.Status})
	}
	return nil // not ours: consumers ignore what they do not know
}

// ApplyEventTx runs ApplyEvent in a transaction of its own for the event's tenant (the double and tests; the
// Postgres consumer binds the inbox transaction instead).
func (s *Service) ApplyEventTx(ctx context.Context, env events.Envelope) error {
	ctx = tenancy.With(ctx, tenancy.Context{TenantID: env.TenantID})
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.ApplyEvent(ctx, tx, env) })
}

// streamScope splits a stream name ("brand:<id>", "outlet:<id>", "tenant:<id>").
func streamScope(stream string) (kind, id string, ok bool) {
	kind, id, ok = strings.Cut(stream, ":")
	return kind, id, ok && id != ""
}

// ApplyOp projects one parsed op through the go-common/sync engine: every mutation of an entity catalog owns
// becomes a candidate, and each row the op touches (or a void exposes) is resolved from its candidates and
// the void set, so the row is a function of the set of ops, whatever their arrival order. Mutations of
// entities another service owns, or of an entity on a stream it does not live in, are ignored (ADR-0002 S13).
func (s *Service) ApplyOp(ctx context.Context, tx repository.Tx, op *gsync.Op) error {
	if op.TenantID == "" {
		return errors.New("service: op without a tenant")
	}
	// tx is bound to one tenant; the stores refuse a key of another (pg) or are per tenant (mem).
	kind, scope, ok := streamScope(op.Ref.Stream)
	if !ok || (kind != model.StreamBrand && kind != model.StreamOutlet) {
		return nil // the tenant stream carries only entities other services own
	}
	if _, err := ids.Parse(scope); err != nil {
		s.log.WarnContext(ctx, "catalog: op on a stream with a malformed id ignored", "stream", kind)
		return nil
	}
	own := *op
	own.Muts = nil
	for _, m := range op.Muts {
		if e, ok := s.reg.Entities[m.E]; ok && e.Stream == kind {
			own.Muts = append(own.Muts, m)
		}
	}
	if len(own.Muts) == 0 && len(own.Voids) == 0 {
		return nil
	}
	if err := tx.LockProjection(ctx); err != nil {
		return err
	}
	store := tx.SyncStore()
	eng := &gsync.Engine{Reg: s.reg, Service: serviceName, Store: store}
	res, err := eng.Apply(ctx, &own)
	if err != nil {
		return err
	}
	for _, r := range res.Rows {
		if err := s.writeRow(ctx, tx, store, r); err != nil {
			return err
		}
	}
	return nil
}

// ApplyOpTx projects one op in a transaction of its own (the example-world seed and tests; the Postgres
// consumer applies events inside the inbox transaction instead).
func (s *Service) ApplyOpTx(ctx context.Context, op *gsync.Op) error {
	ctx = tenancy.With(ctx, tenancy.Context{TenantID: op.TenantID})
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.ApplyOp(ctx, tx, op) })
}

// writeRow stores the resolved row (or removes it when no live insert is left) and refreshes its findings.
func (s *Service) writeRow(ctx context.Context, tx repository.Tx, store gsync.Store, row gsync.Row) error {
	ent := s.reg.Entities[row.Key.Entity]
	all, err := store.Candidates(ctx, row.Key)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	refs := make([]gsync.OpRef, len(all))
	for i, c := range all {
		refs[i] = c.Ref
	}
	voided, err := store.Voided(ctx, row.Key.Tenant, refs)
	if err != nil {
		return err
	}
	var live []gsync.Candidate
	for _, c := range all {
		if !voided[c.Ref] {
			live = append(live, c)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Stamp().Compare(live[j].Stamp()) < 0 })
	sort.Slice(all, func(i, j int) bool { return all[i].Stamp().Compare(all[j].Stamp()) < 0 })
	_, scope, ok := streamScope(all[0].Ref.Stream)
	scopeID, perr := ids.Parse(scope)
	if !ok || perr != nil {
		s.log.WarnContext(ctx, "catalog: op on a stream with a malformed id ignored", "entity", ent.Name)
		return nil
	}
	rk := repository.RowKey{Entity: ent.Name, ID: row.Key.ID, Stream: ent.Stream, ScopeID: scopeID}
	if !row.Exists {
		if err := tx.DeleteRow(ctx, rk); err != nil {
			return err
		}
	} else if err := tx.PutRow(ctx, model.Row{Entity: ent.Name, ID: row.Key.ID, Stream: ent.Stream, ScopeID: scopeID,
		Fields: row.Fields, Stamps: winnerStamps(ent, row.Fields, live), Gone: row.Deleted, UpdatedAt: now(s)}); err != nil {
		return err
	}
	return s.detect(ctx, tx, ent, rk, row.Conflicts, live)
}

// groupFields lists the fields that win together with the named lww field (its declared group, else itself).
func groupFields(ent *gsync.Entity, name string) []string {
	f := ent.Fields[name]
	if f.Group == "" {
		return []string{name}
	}
	var out []string
	for _, n := range ent.FieldNames() {
		if g := ent.Fields[n]; (g.Policy == gsync.PolicyLWW || g.Policy == gsync.PolicyLWWReview) && g.Group == f.Group {
			out = append(out, n)
		}
	}
	return out
}

// carriers returns the live candidates (in stamp order) that write any field of the group.
func carriers(live []gsync.Candidate, names []string) []gsync.Candidate {
	var out []gsync.Candidate
	for _, c := range live {
		if c.Kind != gsync.KindInsert && c.Kind != gsync.KindSet {
			continue
		}
		for _, n := range names {
			if _, ok := c.F[n]; ok {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// winnerStamps maps each lww field of the row to the hlc of the write that won its group; a later edit of a
// review field cites it as `prev`.
func winnerStamps(ent *gsync.Entity, fields map[string]any, live []gsync.Candidate) map[string]string {
	out := map[string]string{}
	for name := range fields {
		if f := ent.Fields[name]; f == nil || (f.Policy != gsync.PolicyLWW && f.Policy != gsync.PolicyLWWReview) {
			continue
		}
		if cs := carriers(live, groupFields(ent, name)); len(cs) > 0 {
			out[name] = cs[len(cs)-1].HLC
		}
	}
	return out
}

// sawPredecessor: the writer of c saw p when, for every review field c carries, its prev is p's hlc.
func sawPredecessor(c, p gsync.Candidate, names []string) bool {
	seen := false
	for _, n := range names {
		if _, ok := c.F[n]; !ok {
			continue
		}
		seen = true
		if c.Prev[n] != p.HLC {
			return false
		}
	}
	return seen
}

// detect derives the price-conflict findings of one row (RM-M4: concurrent edits of a price field keep the
// winner, retain the loser and ask a manager to confirm). A brand-stream row concerns every outlet of the
// brand known to catalog; an outlet-stream row, its outlet. A finding closes as superseded once a later edit
// saw the winner. Conflicts of other review fields (tax class) are resolved by the engine and need no finding.
func (s *Service) detect(ctx context.Context, tx repository.Tx, ent *gsync.Entity, rk repository.RowKey, conflicts []gsync.Conflict, live []gsync.Candidate) error {
	var keep []string
	var outlets []ids.ID
	for _, c := range conflicts {
		var field string
		for _, n := range c.Fields {
			if f := ent.Fields[n]; f != nil && f.Type == "paise" {
				field = n
			}
		}
		if field == "" {
			continue
		}
		if outlets == nil {
			if rk.Stream == model.StreamOutlet {
				outlets = []ids.ID{rk.ScopeID}
			} else {
				all, err := tx.ListOutlets(ctx, allOutlets)
				if err != nil {
					return err
				}
				outlets = []ids.ID{}
				for _, o := range all {
					if o.BrandID == rk.ScopeID {
						outlets = append(outlets, o.ID)
					}
				}
			}
		}
		cs := carriers(live, c.Fields)
		wi := slices.IndexFunc(cs, func(x gsync.Candidate) bool { return x.Stamp() == c.Winner })
		if wi < 0 {
			continue
		}
		status := model.StatusOpen
		for j := wi + 1; j < len(cs); j++ {
			if sawPredecessor(cs[j], cs[j-1], c.Fields) {
				status = model.StatusSuperseded
				break
			}
		}
		for _, o := range outlets {
			dk := strings.Join([]string{model.KindPriceConflict, rk.Entity, rk.ID, field, cs[wi].Ref.String(), o.String()}, "|")
			keep = append(keep, dk)
			e := model.Exception{ID: s.newID(), Kind: model.KindPriceConflict, Severity: "warning", Status: status, SubjectKind: rk.Entity,
				SubjectID: rk.ID, OutletID: o, DetectedAt: now(s), DetailKey: "exception.price_conflict", DedupeKey: dk,
				Params: map[string]any{"kept_paise": asInt(cs[wi].F[field]), "lost_paise": asInt(c.LoserValues[field])}}
			if status == model.StatusSuperseded {
				t := now(s)
				e.ResolvedAt = &t
			}
			if err := tx.UpsertException(ctx, e); err != nil {
				return err
			}
		}
	}
	return tx.SupersedeExceptions(ctx, rk.Entity, rk.ID, keep, now(s))
}

func asInt(v any) int64 {
	n, _ := v.(int64)
	return n
}

// Repo exposes the repository to the example-world seed and tests.
func (s *Service) Repo() repository.Repository { return s.repo }
