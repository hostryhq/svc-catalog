//go:build integration

package api_test

import (
	"context"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/httpsuite"
	"github.com/hostryhq/svc-catalog/internal/itest"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

func column(t testing.TB, s *itest.Stack, query string, tenant ids.ID) []string {
	t.Helper()
	rows, err := s.DB.Admin.QueryContext(context.Background(), query, tenant.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// factory starts the real service on a fresh database as a suite target.
func factory(t *testing.T) *httpsuite.Target {
	t.Helper()
	s := itest.New(t)
	return &httpsuite.Target{
		Base: s.Srv.URL, Tokens: s.Tokens,
		Load: func(t testing.TB, w seed.World, world string) {
			if err := w.Apply(context.Background(), s.App.Service, world); err != nil {
				t.Fatal(err)
			}
		},
		Emitted:        s.Gateway.Stored,
		SetGatewayDown: s.Gateway.SetDown,
		Settle:         func(testing.TB) { s.Settle(context.Background()) },
		EventTypes: func(t testing.TB, tenant ids.ID) []string {
			return column(t, s, `SELECT type FROM outbox WHERE tenant_id = $1 ORDER BY seq`, tenant)
		},
		AuditActions: func(t testing.TB, tenant ids.ID) []string {
			return column(t, s, `SELECT action FROM audit_log WHERE tenant_id = $1::uuid ORDER BY id`, tenant)
		},
		VendorLog: func(t testing.TB, tenant ids.ID) []string {
			return column(t, s, `SELECT action_key FROM vendor_access_log WHERE tenant_id = $1::uuid ORDER BY id`, tenant)
		},
	}
}

func TestRealIsolation(t *testing.T)     { httpsuite.Isolation(t, factory) }
func TestRealOutletScope(t *testing.T)   { httpsuite.OutletScope(t, factory) }
func TestRealAuthorization(t *testing.T) { httpsuite.Authorization(t, factory) }
func TestRealTaxClass(t *testing.T)      { httpsuite.TaxClass(t, factory) }
func TestRealImport(t *testing.T)        { httpsuite.Import(t, factory) }
func TestRealOutage(t *testing.T)        { httpsuite.Outage(t, factory) }
func TestRealImportBrand(t *testing.T)   { httpsuite.ImportBrand(t, factory) }
func TestRealSupportImport(t *testing.T) { httpsuite.SupportImport(t, factory) }
