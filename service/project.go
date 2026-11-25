package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/repository"
)

// Event types catalog consumes.
const (
	EventOpStored      = "sync.op.stored" // proof-of-concept name; sync.op.<namespace> once ADR-0002 A2.3 fixes it
	EventOutletCreated = "tenancy.outlet.created"
	EventOutletUpdated = "tenancy.outlet.updated"
)

// ErrOpBodyUnavailable is returned when an op event carries no body and no fetcher is configured: the event is
// retried and then parked (docs/notes.md, "ADR needed: op bodies on the bus").
var ErrOpBodyUnavailable = errors.New("service: op event carries no body")

// OpBodyFetcher reads the canonical body of a stored op from the gateway when the event does not carry it.
type OpBodyFetcher interface {
	Body(ctx context.Context, tenant ids.ID, origin, stream string, seq int64) (string, error)
}

// opEventData is the payload of a sync op event. Body (the canonical JSON text of the op body, byte for byte
// as the gateway stored it) is the one field the proof-of-concept event lacks.
type opEventData struct {
	Origin string `json:"origin"`
	Stream string `json:"stream"`
	Seq    int64  `json:"seq"`
	H      string `json:"h"`
	Intent string `json:"intent"`
	Status string `json:"status"`
	Body   string `json:"body"`
}

// ApplyEvent applies one bus event inside tx (the tenant context of ctx is the event's tenant, and tx is bound
// to it). It is idempotent: an op already applied, or voided before it arrived, changes nothing.
func (s *Service) ApplyEvent(ctx context.Context, tx repository.Tx, env events.Envelope, fetch OpBodyFetcher) error {
	switch {
	case strings.HasPrefix(env.Type, "sync.op."):
		var d opEventData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return fmt.Errorf("service: op event data: %w", err)
		}
		if d.Status != "" && d.Status != "accepted" && d.Status != "flagged" {
			return nil // quarantined, voided and held ops are not applied (ADR-0002 S11)
		}
		if kind, _, ok := proj.StreamScope(d.Stream); !ok || kind == "tenant" {
			return nil // the tenant stream carries only entities other services own
		}
		body := d.Body
		if body == "" {
			if fetch == nil {
				return ErrOpBodyUnavailable
			}
			var err error
			if body, err = fetch.Body(ctx, env.TenantID, d.Origin, d.Stream, d.Seq); err != nil {
				return err
			}
		}
		op, err := proj.ParseBody(body)
		if err != nil {
			return err
		}
		if op.Origin != d.Origin || op.Stream != d.Stream || op.Seq != d.Seq {
			return fmt.Errorf("service: op body identity does not match its event")
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
func (s *Service) ApplyEventTx(ctx context.Context, env events.Envelope, fetch OpBodyFetcher) error {
	ctx = tenancy.With(ctx, tenancy.Context{TenantID: env.TenantID})
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.ApplyEvent(ctx, tx, env, fetch) })
}

// ApplyOp projects one op. Order does not matter: the row is derived from the whole set of its effects.
func (s *Service) ApplyOp(ctx context.Context, tx repository.Tx, op proj.Op) error {
	kind, scope, ok := proj.StreamScope(op.Stream)
	if !ok || (kind != model.StreamBrand && kind != model.StreamOutlet) {
		return nil
	}
	scopeID, err := ids.Parse(scope)
	if err != nil {
		s.log.WarnContext(ctx, "catalog: op on a stream with a malformed id ignored", "stream", kind)
		return nil
	}
	if err := tx.LockProjection(ctx); err != nil {
		return err
	}
	key := op.Key()
	seen, _, err := tx.OpState(ctx, key)
	if err != nil || seen {
		return err // idempotent apply: an op applied twice, or voided before it arrived, does nothing
	}
	if err := tx.PutOp(ctx, key, op.Stream, op.Intent, false, s.clock.Now()); err != nil {
		return err
	}
	if op.Intent == proj.IntentVoid {
		return s.applyVoids(ctx, tx, op)
	}
	var fx []repository.EffectRec
	touched := map[repository.RowKey]bool{}
	var order []repository.RowKey
	for i, m := range op.Muts {
		def, ok := proj.Entities[m.E]
		if !ok || def.Stream != kind || m.ID == "" || len(m.ID) > 64 {
			continue // an entity of another owner, or unknown to this release (ADR-0002 S13): ignored
		}
		switch m.K {
		case proj.KInsert, proj.KSet, proj.KDel, proj.KUndel:
		default:
			continue
		}
		rk := repository.RowKey{Entity: m.E, ID: m.ID, Stream: kind, ScopeID: scopeID}
		fx = append(fx, repository.EffectRec{Entity: m.E, RowID: m.ID, Stream: kind, ScopeID: scopeID, Effect: proj.Effect{
			Op: key, St: proj.Stamp{HLC: op.HLC, Origin: op.Origin, Seq: op.Seq, Idx: i}, Kind: m.K, F: m.F, Prev: m.Prev}})
		if !touched[rk] {
			touched[rk] = true
			order = append(order, rk)
		}
	}
	if err := tx.AddEffects(ctx, fx); err != nil {
		return err
	}
	return s.rederive(ctx, tx, order)
}

func (s *Service) applyVoids(ctx context.Context, tx repository.Tx, op proj.Op) error {
	var touched []repository.RowKey
	for _, m := range op.Muts {
		if m.E != proj.EntityVoid {
			continue
		}
		origin, _ := m.F["origin"].(string)
		stream, _ := m.F["stream"].(string)
		seq, _ := m.F["seq"].(int64)
		if origin == "" || stream == "" || seq < 1 {
			continue
		}
		rows, err := tx.VoidOp(ctx, proj.Ref{Origin: origin, Stream: stream, Seq: seq}.Key(), s.clock.Now())
		if err != nil {
			return err
		}
		touched = append(touched, rows...)
	}
	return s.rederive(ctx, tx, touched)
}

// rederive recomputes each touched row from its effects, stores or deletes it and refreshes its price-conflict
// findings.
func (s *Service) rederive(ctx context.Context, tx repository.Tx, keys []repository.RowKey) error {
	seen := map[repository.RowKey]bool{}
	for _, rk := range keys {
		if seen[rk] {
			continue
		}
		seen[rk] = true
		fx, err := tx.EffectsOf(ctx, rk)
		if err != nil {
			return err
		}
		d := proj.Derive(fx)
		if !d.Exists {
			if err := tx.DeleteRow(ctx, rk); err != nil {
				return err
			}
		} else if err := tx.PutRow(ctx, model.Row{Entity: rk.Entity, ID: rk.ID, Stream: rk.Stream, ScopeID: rk.ScopeID,
			Fields: d.Fields, Stamps: d.Stamps, Gone: d.Gone, UpdatedAt: s.clock.Now().UTC()}); err != nil {
			return err
		}
		if err := s.detect(ctx, tx, rk, fx); err != nil {
			return err
		}
	}
	return nil
}

// detect derives the price-conflict findings of one row (RM-M4: concurrent edits of a price field keep the
// winner, retain the loser and ask a manager to confirm). A brand-stream row concerns every outlet of the
// brand known to catalog; an outlet-stream row, its outlet.
func (s *Service) detect(ctx context.Context, tx repository.Tx, rk repository.RowKey, fx []proj.Effect) error {
	def := proj.Entities[rk.Entity]
	var keep []string
	conflicts := proj.Conflicts(rk.Entity, fx)
	if len(conflicts) > 0 {
		var outlets []ids.ID
		if rk.Stream == model.StreamOutlet {
			outlets = []ids.ID{rk.ScopeID}
		} else {
			all, err := tx.ListOutlets(ctx, allOutlets)
			if err != nil {
				return err
			}
			for _, o := range all {
				if o.BrandID == rk.ScopeID {
					outlets = append(outlets, o.ID)
				}
			}
		}
		for _, c := range conflicts {
			if !def.PriceFields[c.Field] {
				continue
			}
			status := model.StatusOpen
			if c.Superseded {
				status = model.StatusSuperseded
			}
			for _, o := range outlets {
				dk := strings.Join([]string{model.KindPriceConflict, rk.Entity, rk.ID, c.Field, c.Later.Op, o.String()}, "|")
				keep = append(keep, dk)
				e := model.Exception{ID: s.newID(), Kind: model.KindPriceConflict, Severity: "warning", Status: status, SubjectKind: rk.Entity,
					SubjectID: rk.ID, OutletID: o, DetectedAt: s.clock.Now().UTC(), DetailKey: "exception.price_conflict", DedupeKey: dk,
					Params: map[string]any{"kept_paise": asInt(c.Later.F[c.Field]), "lost_paise": asInt(c.Earlier.F[c.Field])}}
				if status == model.StatusSuperseded {
					t := s.clock.Now().UTC()
					e.ResolvedAt = &t
				}
				if err := tx.UpsertException(ctx, e); err != nil {
					return err
				}
			}
		}
	}
	return tx.SupersedeExceptions(ctx, rk.Entity, rk.ID, keep, s.clock.Now().UTC())
}

func asInt(v any) int64 {
	n, _ := v.(int64)
	return n
}

// ApplyOpTx projects one op in a transaction of its own for tenant (the double's loopback emitter and the
// example-world seed; the Postgres consumer applies events inside the inbox transaction instead).
func (s *Service) ApplyOpTx(ctx context.Context, tenant ids.ID, op proj.Op) error {
	ctx = tenancy.With(ctx, tenancy.Context{TenantID: tenant})
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.ApplyOp(ctx, tx, op) })
}

// Repo exposes the repository to the example-world seed and tests.
func (s *Service) Repo() repository.Repository { return s.repo }
