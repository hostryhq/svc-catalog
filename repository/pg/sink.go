package pg

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/auditsink"
)

// AuditSink writes the vendor_access_log row of a support-token request in the transaction of the action
// (ADR-0013 V5). If the row cannot be written the request fails and nothing changes. A refused request is
// recorded in a short transaction of its own. Catalog publishes no event for these rows (docs/notes.md).
type AuditSink struct {
	DB  *sql.DB // the application pool
	Log *slog.Logger
}

var _ auth.VendorAuditSink = (*AuditSink)(nil)

func insertEntry(ctx context.Context, tx tenancy.Execer, tenant ids.ID, e auth.VendorAccessEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO vendor_access_log (tenant_id, id, occurred_at, vendor_user, token_type, support_session_id, action_key, target_id, ticket_reference, outcome, result_count, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, tenant, e.ID, e.Time.UTC(), e.VendorUser, "support", nullStr(e.SupportSession),
		auditsink.ActionKey(e.ActionKey), nullStr(e.TargetID), nullStr(e.Ticket), e.Outcome, e.ResultCount, e.RequestID)
	return err
}

// RecordInTx implements auth.VendorAuditSink.
func (s *AuditSink) RecordInTx(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		return err
	}
	return insertEntry(ctx, x, tid, e)
}

// RecordRefused implements auth.VendorAuditSink.
func (s *AuditSink) RecordRefused(ctx context.Context, e auth.VendorAccessEntry) error {
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		s.Log.WarnContext(ctx, "support request refused before an organisation was named", "action", e.ActionKey)
		return nil
	}
	e.Outcome = auth.OutcomeRefused
	c := tenancy.With(context.WithoutCancel(ctx), tenancy.Context{TenantID: tid, ActorID: e.VendorUser})
	return tenancy.WithTx(c, s.DB, func(c context.Context, tx *sql.Tx) error { return insertEntry(c, tx, tid, e) })
}
