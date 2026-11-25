package double_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/contractenv"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

func client(env *double.Env) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &tokenTransport{tok: env.TenantToken(seed.Example.Org())}}
}

type tokenTransport struct{ tok string }

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// TestContractExamples runs every operation against the double after SeedWorld has loaded the operation's
// example world; the double must serve the spec's own examples, byte for byte as JSON.
func TestContractExamples(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	contracttest.Run(t, contracttest.Options{SpecPath: contractenv.SpecPath(t), BaseURL: srv.URL, Client: client(env), ExpectExamples: true, SeedWorld: env.SeedWorld})
}
