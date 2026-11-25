package double_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/itest"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

func TestInjectedFailuresReachCallers(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.SeedWorld(context.Background(), seed.Base); err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	h := itest.Bearer(env.TenantToken(seed.Example.Org()))
	remove := env.Inject(double.Fault{Match: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/menu") }, Status: 503})
	if r := itest.Do(t, srv.URL, "GET", "/v1/menu", h, nil); r.Status != 503 || r.Code() != "injected" {
		t.Fatalf("injected status expected: %d %s", r.Status, r.Body)
	}
	if r := itest.Do(t, srv.URL, "GET", "/v1/items", h, nil); r.Status != 200 {
		t.Fatalf("other routes are unaffected: %d", r.Status)
	}
	remove()
	if r := itest.Do(t, srv.URL, "GET", "/v1/items", h, nil); r.Status != 200 {
		t.Fatalf("removed fault: %d", r.Status)
	}
	// A hang is a timeout for the caller.
	env.Inject(double.Fault{Match: func(*http.Request) bool { return true }, Hang: true})
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(srv.URL + "/v1/items"); err == nil {
		t.Fatal("a hang must time out the caller")
	}
}

// A gateway that is down must surface as 503 emit_unavailable, never as a silent success.
func TestCloudWritesAnswer503WithoutAGateway(t *testing.T) {
	tk := double.NewTokens()
	d, err := double.New(double.Config{Tenant: tk.TenantIssuer.Verifier(), NoEmitter: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SeedWorld(context.Background(), seed.Base); err != nil {
		t.Fatal(err)
	}
	srv := d.Server()
	defer srv.Close()
	r := itest.Do(t, srv.URL, "PUT", "/v1/items/"+seed.Example.Item().String()+"/tax-class", itest.Bearer(tk.TenantToken(seed.Example.Org())),
		map[string]any{"tax_class": "exempt", "reason": "Gateway is down."})
	if r.Status != 503 || r.Code() != "emit_unavailable" {
		t.Fatalf("got %d %s", r.Status, r.Body)
	}
}
