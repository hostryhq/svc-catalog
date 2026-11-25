// Package itest assembles the real service (Postgres-backed) for integration tests: a fresh database on the
// local dev stack (internal/pgtest), the wired handler, a fixed clock for entity timestamps and the test token
// issuer. The sync gateway is replaced by a loopback emitter, as in the double. Other services' integration
// tests use the double instead.
package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/app"
	"github.com/hostryhq/svc-catalog/internal/pgtest"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/service"
)

// Loopback is the gateway stand-in: it records the op and applies it through the projection, as the bus would.
type Loopback struct {
	mu    sync.Mutex
	Svc   *service.Service
	Clock clock.Clock
	Ops   []double.Emitted
	keys  map[string]string
	seq   map[string]int64
}

// Emit implements service.Emitter.
func (l *Loopback) Emit(ctx context.Context, tenant ids.ID, e service.Emission) (service.Emitted, error) {
	l.mu.Lock()
	if l.keys == nil {
		l.keys, l.seq = map[string]string{}, map[string]int64{}
	}
	if id, ok := l.keys[tenant.String()+e.Key]; ok && e.Key != "" {
		l.mu.Unlock()
		return service.Emitted{OpID: id}, nil
	}
	l.seq[tenant.String()+e.Stream]++
	n := l.seq[tenant.String()+e.Stream]
	id := fmt.Sprintf("cloud-1/%s/%d", e.Stream, n)
	l.keys[tenant.String()+e.Key] = id
	l.Ops = append(l.Ops, double.Emitted{Tenant: tenant, Emission: e, OpID: id})
	l.mu.Unlock()
	op := proj.Op{Ref: proj.Ref{Origin: "cloud-1", Stream: e.Stream, Seq: n}, HLC: uint64(l.Clock.Now().UnixMilli())<<16 + uint64(n), Intent: e.Intent, Muts: e.Muts}
	return service.Emitted{OpID: id}, l.Svc.ApplyOpTx(context.WithoutCancel(ctx), tenant, op)
}

// Stack is the real service over a fresh database.
type Stack struct {
	DB       *pgtest.DB
	App      *app.App
	Srv      *httptest.Server
	Clock    *testkit.FakeClock
	Loopback *Loopback
	*double.Tokens
}

// New starts a stack.
func New(t testing.TB) *Stack {
	t.Helper()
	db := pgtest.New(t)
	clk := testkit.NewClock()
	clk.Set(seed.At)
	tk := double.NewTokens()
	lb := &Loopback{Clock: clk}
	a, err := app.Build(app.Options{AppDB: db.App, Tenant: tk.TenantIssuer.Verifier(), Services: tk.Registry.Verifier("catalog"), Clock: clk, AuthClock: clock.System,
		IDs: ids.NewGenerator(clk.Now, nil), Log: slog.New(slog.DiscardHandler), Emitter: lb})
	if err != nil {
		t.Fatal(err)
	}
	lb.Svc = a.Service
	s := &Stack{DB: db, App: a, Srv: httptest.NewServer(a.Handler), Clock: clk, Loopback: lb, Tokens: tk}
	t.Cleanup(s.Srv.Close)
	return s
}

// SeedWorld empties the database and stores the named example world (contracttest Options.SeedWorld).
func (s *Stack) SeedWorld(ctx context.Context, world string) error {
	if err := s.DB.Truncate(ctx); err != nil {
		return err
	}
	return seed.Example.Apply(ctx, s.App.Service, world)
}

// Response is a decoded HTTP answer.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// JSON decodes the body into a map.
func (r Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// Code returns the problem code of an error response ("" otherwise).
func (r Response) Code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &p)
	return p.Code
}

// Do sends a request; body may be nil, a string, []byte or any JSON-marshalable value. The Idempotency-Key
// header is added for mutating methods unless the headers already carry one.
func Do(t testing.TB, base, method, path string, headers http.Header, body any) Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && headers.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", ids.New().String())
	}
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// Bearer returns headers with a bearer token.
func Bearer(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	return h
}

// Merge combines header sets (later wins).
func Merge(hs ...http.Header) http.Header {
	out := http.Header{}
	for _, h := range hs {
		for k, vs := range h {
			out[k] = vs
		}
	}
	return out
}
