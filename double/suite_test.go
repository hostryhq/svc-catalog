package double_test

import (
	"context"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/httpsuite"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

// factory starts a double as a suite target.
func factory(t *testing.T) *httpsuite.Target {
	t.Helper()
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	t.Cleanup(srv.Close)
	return &httpsuite.Target{
		Base: srv.URL, Tokens: env.Tokens,
		Load: func(t testing.TB, w seed.World, world string) {
			if err := env.Load(context.Background(), w, world); err != nil {
				t.Fatal(err)
			}
		},
		Emitted:        env.Emitted,
		SetGatewayDown: env.SetGatewayDown,
		Settle:         func(testing.TB) { env.Settle(context.Background()) },
		EventTypes: func(_ testing.TB, tenant ids.ID) []string {
			var out []string
			for _, e := range env.Store.Events(tenant) {
				out = append(out, e.Type)
			}
			return out
		},
		AuditActions: func(_ testing.TB, tenant ids.ID) []string {
			var out []string
			for _, e := range env.Store.AuditLog(tenant) {
				out = append(out, e.Action)
			}
			return out
		},
		VendorLog: func(_ testing.TB, tenant ids.ID) []string {
			var out []string
			for _, e := range env.Store.VendorLog(tenant) {
				out = append(out, e.ActionKey)
			}
			return out
		},
	}
}

func TestDoubleIsolation(t *testing.T)     { httpsuite.Isolation(t, factory) }
func TestDoubleOutletScope(t *testing.T)   { httpsuite.OutletScope(t, factory) }
func TestDoubleAuthorization(t *testing.T) { httpsuite.Authorization(t, factory) }
func TestDoubleTaxClass(t *testing.T)      { httpsuite.TaxClass(t, factory) }
func TestDoubleImport(t *testing.T)        { httpsuite.Import(t, factory) }
func TestDoubleOutage(t *testing.T)        { httpsuite.Outage(t, factory) }
func TestDoubleImportBrand(t *testing.T)   { httpsuite.ImportBrand(t, factory) }
