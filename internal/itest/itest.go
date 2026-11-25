// Package itest assembles the real service (Postgres-backed) for integration tests: a fresh database on the
// local dev stack (internal/pgtest), the wired handler, a fixed clock for entity timestamps and the test token
// issuer. The sync gateway is replaced by an in-process fake of its emit operation (internal/fakegw), called
// over HTTP by the real emit client and emit_queue worker, as in the double. Other services' integration tests
// use the double instead.
package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/app"
	"github.com/hostryhq/svc-catalog/internal/fakegw"
	"github.com/hostryhq/svc-catalog/internal/pgtest"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

// Stack is the real service over a fresh database.
type Stack struct {
	DB    *pgtest.DB
	App   *app.App
	Srv   *httptest.Server
	Clock *testkit.FakeClock
	// Gateway is the in-process fake of the sync gateway; GatewaySrv serves its emit operation over HTTP, which
	// the real emit client and emit_queue worker call.
	Gateway    *fakegw.Gateway
	GatewaySrv *httptest.Server
	Queue      *emit.Queue
	IDs        *seed.IDs
	*double.Tokens

	mu      sync.Mutex
	pending []events.Envelope
	skew    *skewClock
}

// skewClock is the queue worker's clock: real time plus an offset the stack moves forward, so rows that backed
// off while the gateway was down are due at the next Settle without waiting.
type skewClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *skewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off).UTC()
}

func (c *skewClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// New starts a stack.
func New(t testing.TB) *Stack {
	t.Helper()
	db := pgtest.New(t)
	clk := testkit.NewClock()
	clk.Set(seed.At)
	tk := double.NewTokens()
	gw, err := fakegw.New(clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	gsrv := httptest.NewServer(gw)
	t.Cleanup(gsrv.Close)
	client := &emit.Client{BaseURL: gsrv.URL, Token: func(context.Context, ids.ID) (string, error) { return "test-service-token", nil }}
	var skew skewClock
	idsrc := seed.NewIDs(ids.NewGenerator(clk.Now, nil))
	a, err := app.Build(app.Options{AppDB: db.App, Tenant: tk.TenantIssuer.Verifier(), Services: tk.Registry.Verifier("catalog"), Clock: clk, AuthClock: clock.System,
		IDs: idsrc, Log: slog.New(slog.DiscardHandler), Emitter: client})
	if err != nil {
		t.Fatal(err)
	}
	s := &Stack{DB: db, skew: &skew, App: a, Clock: clk, IDs: idsrc, Gateway: gw, GatewaySrv: gsrv, Tokens: tk,
		Queue: &emit.Queue{DB: db.Relay, Emitter: client, Log: slog.New(slog.DiscardHandler), Clock: &skew}}
	// Stored ops reach the projection as bus events, after the emitting request: see Settle.
	gw.Deliver = func(_ context.Context, ev events.Envelope) error {
		s.mu.Lock()
		s.pending = append(s.pending, ev)
		s.mu.Unlock()
		return nil
	}
	s.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler.ServeHTTP(w, r)
		s.Settle(context.WithoutCancel(r.Context()))
	}))
	t.Cleanup(s.Srv.Close)
	return s
}

// Settle runs the emit worker once (queued ops go to the gateway over HTTP, in order) and delivers every op the
// gateway stored to the projection through the bus consumer path (ParseOp, in a transaction).
func (s *Stack) Settle(ctx context.Context) {
	s.skew.advance(time.Hour)
	if _, err := s.Queue.Drain(ctx); err != nil {
		slog.Error("itest: emit queue drain", "error", err)
	}
	s.mu.Lock()
	p := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, ev := range p {
		tx, err := s.DB.App.BeginTx(ctx, nil)
		if err == nil {
			if err = s.App.Consume(ctx, tx, ev); err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		if err != nil {
			slog.Error("itest: delivery failed", "error", err)
		}
	}
}

// SeedWorld empties the database and stores the named example world (contracttest Options.SeedWorld).
func (s *Stack) SeedWorld(ctx context.Context, world string) error {
	if err := s.DB.Truncate(ctx); err != nil {
		return err
	}
	s.Gateway.Reset()
	s.Clock.Set(seed.At)
	if err := seed.Example.Apply(ctx, s.App.Service, world); err != nil {
		return err
	}
	s.IDs.Next(seed.Example, world)
	s.Clock.Set(seed.At.Add(seed.ClockAdvance(world)))
	return nil
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
