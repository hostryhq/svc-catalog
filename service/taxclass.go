package service

import (
	"context"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/repository"
)

// IntentTaxClass is the intent of the emitted op. The intent registry (api-specs/sync/intents) is a separate
// deliverable; the gateway PoC accepts cloud-origin ops under any config intent (docs/notes.md).
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
		em := Emission{Stream: "brand:" + row.ScopeID.String(), Intent: IntentTaxClass, Muts: []proj.Mut{{
			E: entity, ID: row.ID, K: proj.KSet, F: map[string]any{field: class}, Prev: prevStamp(row, field)}}}
		if im, ok := IdemFrom(ctx); ok {
			em.Key = "taxclass|" + im.Op + "|" + im.Key
		}
		out, err := s.emit(ctx, tenant, em)
		if err != nil {
			return TaxChange{}, err
		}
		at := now(s)
		if err := tx.AppendAudit(ctx, model.AuditEntry{ID: s.newID(), At: at, Actor: actor(ctx), Action: PermTaxClass, SubjectKind: entity,
			SubjectID: row.ID, Before: row.Str(field), After: class, Reason: reason}); err != nil {
			return TaxChange{}, err
		}
		return TaxChange{ID: row.ID, TaxClass: class, UpdatedAt: at, OpID: out.OpID}, nil
	})
}

// prevStamp is the `prev` of a review-field write: the stamp of the value the editor saw.
func prevStamp(r model.Row, field string) map[string]string {
	if st := r.Stamps[field]; st != "" {
		return map[string]string{field: st}
	}
	return nil
}

func (s *Service) emit(ctx context.Context, tenant ids.ID, e Emission) (Emitted, error) {
	if s.emitter == nil {
		return Emitted{}, ErrEmitUnavailable
	}
	out, err := s.emitter.Emit(ctx, tenant, e)
	if err != nil {
		s.log.ErrorContext(ctx, "catalog: emit failed", "intent", e.Intent, "error", err)
		return Emitted{}, ErrEmitUnavailable
	}
	return out, nil
}
