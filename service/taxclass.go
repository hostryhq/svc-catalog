package service

import (
	"context"
	"errors"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
)

// IntentTaxClass is the cloud-origin intent of a strong-session tax-class edit (api-specs sync/intents
// cloud.yaml, ADR-0014 A2.3).
const IntentTaxClass = "menu.tax_class.set"

// SetCategoryTaxClass sets a category's default tax class. Strong session only (the guard refuses pin and
// device sessions and stale OTPs); online only; audited. Existing items keep their own class (RM-M5).
func (s *Service) SetCategoryTaxClass(ctx context.Context, id ids.ID, class, reason string) (TaxChange, error) {
	return s.setTaxClass(ctx, "category", "default_tax_class", id, class, reason)
}

// SetItemTaxClass sets an item's tax class. Lines already on any bill keep the class they were added with
// (RM-M6): bills snapshot it, so nothing here touches them.
func (s *Service) SetItemTaxClass(ctx context.Context, id ids.ID, class, reason string) (TaxChange, error) {
	return s.setTaxClass(ctx, "item", "tax_class", id, class, reason)
}

func (s *Service) setTaxClass(ctx context.Context, entity, field string, id ids.ID, class, reason string) (TaxChange, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return TaxChange{}, err
	}
	if err := s.validTaxClass("tax_class", class); err != nil {
		return TaxChange{}, err
	}
	if reason == "" {
		return TaxChange{}, model.Invalid("reason", "required", "a reason is required")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (TaxChange, error) {
		sc, _, err := s.scope(ctx, tx, PermTaxClass, "")
		if err != nil {
			return TaxChange{}, err
		}
		brands, err := brandsFor(ctx, tx, sc, false)
		if err != nil {
			return TaxChange{}, err
		}
		row, err := tx.GetRow(ctx, repository.RowQuery{Entity: entity, Brands: brands}, id.String())
		if err != nil {
			return TaxChange{}, repoErr(err)
		}
		perm := PermTaxClass
		op := emit.Op{Stream: "brand:" + row.ScopeID.String(), Intent: IntentTaxClass, IntentV: 1, Actor: emitActor(ctx), AuthorisedBy: &perm,
			Muts: []gsync.Mut{{E: entity, ID: row.ID, K: gsync.KindSet, F: map[string]any{field: class}, Prev: prevStamp(row, field)}}}
		// The key derives from the caller's Idempotency-Key (per operation), so a retried edit is one op at the
		// gateway even when the first call landed (ADR-0014 A2.2 item 4).
		if im, ok := IdemFrom(ctx); ok {
			op.Key = emit.DeriveKey(im.Op+"|"+im.Key, 0)
		} else {
			op.Key = emit.DeterministicKey("taxclass", s.newID().String())
		}
		out, err := s.emitOne(ctx, tenant, op)
		if err != nil {
			return TaxChange{}, err
		}
		at := now(s)
		if err := tx.AppendAudit(ctx, model.AuditEntry{ID: s.newID(), At: at, Actor: actor(ctx), Action: PermTaxClass, SubjectKind: entity,
			SubjectID: row.ID, Before: row.Str(field), After: class, Reason: reason}); err != nil {
			return TaxChange{}, err
		}
		return TaxChange{ID: row.ID, TaxClass: class, UpdatedAt: at, OpID: OpID(out)}, nil
	})
}

// prevStamp is the `prev` of a review-field write: the stamp of the value the editor saw.
func prevStamp(r model.Row, field string) map[string]string {
	if st := r.Stamps[field]; st != "" {
		return map[string]string{field: st}
	}
	return nil
}

// emitActor is the actor of an emitted op: the member, or "support:<vendor user>" for a support session.
func emitActor(ctx context.Context) string {
	if p, ok := auth.PrincipalFrom(ctx); ok && p.Subject != "" {
		return p.Subject
	}
	return actor(ctx)
}

// emitOne sends one op through the gateway emit client. A gateway that is down is *emit.UnavailableError (503
// emit.unavailable, nothing changed); a refusal is *emit.RejectedError, a defect here (the API answers 500
// emit.rejected). Neither is wrapped, so the API layer maps them.
func (s *Service) emitOne(ctx context.Context, tenant ids.ID, op emit.Op) (emit.Result, error) {
	if s.emitter == nil {
		return emit.Result{}, emit.NewUnavailable(errors.New("no emitter configured"), 0)
	}
	res, err := s.emitter.Emit(ctx, tenant, []emit.Op{op})
	if err != nil {
		s.log.ErrorContext(ctx, "catalog: emit failed", "intent", op.Intent, "error", err)
		return emit.Result{}, err
	}
	return res[0], nil
}
