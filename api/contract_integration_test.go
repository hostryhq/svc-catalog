//go:build integration

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-catalog/internal/contractenv"
	"github.com/hostryhq/svc-catalog/internal/itest"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

type tokenTransport struct{ tok string }

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// TestRealServiceContract runs the shared contract suite against the real service on Postgres, the same suite
// and the same example worlds the double passes.
func TestRealServiceContract(t *testing.T) {
	s := itest.New(t)
	contracttest.Run(t, contracttest.Options{
		SpecPath: contractenv.SpecPath(t), BaseURL: s.Srv.URL,
		Client:    &http.Client{Timeout: 10 * time.Second, Transport: &tokenTransport{tok: s.TenantToken(seed.Example.Org())}},
		SeedWorld: s.SeedWorld,
	})
}
