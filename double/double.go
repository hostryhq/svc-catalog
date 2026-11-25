// Package double is the in-memory test double of the catalog service. It runs the real API layer and service
// over an in-memory repository, so it implements the whole public contract (scoping, idempotency, the
// projection of synced ops, menu import) with deterministic ids and time. Other services' tests import it
// instead of the real service. It is verified by the shared contract suite (api-specs/contracttest).
//
// Cloud-side writes go to an in-process fake of the sync gateway's emit operation: a tax-class edit is emitted
// at once, a menu import commit waits in an in-memory emit_queue that a worker drains in order. Each stored op
// returns to the projection as a sync.op event through go-common/sync.ParseOp, as the bus delivers it; ApplyOp
// lets a test deliver a device op.
// Authentication uses the same go-auth verifiers as the real service: give it the test issuers of
// go-auth/authtest. Failure injection (error status, delay, hang) lets callers test their resilience.
package double

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-catalog/api"
	"github.com/hostryhq/svc-catalog/internal/fakegw"
	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/repository/mem"
	"github.com/hostryhq/svc-catalog/service"
)

// Config configures the double. Tenant is required; the rest has defaults.
type Config struct {
	Tenant   *auth.Verifier        // tenant-realm tokens (authtest.NewTenantIssuer(...).Verifier())
	Services *auth.ServiceVerifier // service tokens; support sessions travel with the admin service token (nil: refused)
	// Clock defaults to a fixed instant, the instant of the spec examples (plus the world's ClockAdvance).
	Clock clock.Clock
	// IDs defaults to a generator driven by the clock with a counter, so ids are deterministic.
	IDs *ids.Generator
	// NoEmitter starts with the sync gateway down: interactive cloud-side writes (tax class) answer 503
	// emit.unavailable and queued ones (menu import) wait. SetGatewayDown(false) brings it back.
	NoEmitter bool
}

// Fault is an injected failure for matching requests.
type Fault struct {
	Match  func(*http.Request) bool
	Status int           // respond with this status and a problem body (0: pass through)
	Delay  time.Duration // sleep before answering
	Hang   bool          // never answer until the request is cancelled (a timeout for the caller)
}

// Emitted is one op the sync gateway fake stored: what the bus carries to subscribers.
type Emitted = fakegw.Sealed

// Double is the running double.
type Double struct {
	Store *mem.Store
	Svc   *service.Service
	h     http.Handler
	gw    *fakegw.Gateway
	ids   *seed.IDs
	base  clock.Clock

	mu      sync.Mutex
	fault   []Fault
	offset  time.Duration
	pending []events.Envelope
}

func (d *Double) now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.base.Now().Add(d.offset)
}

// New builds a double. The store is empty; call SeedWorld to match the spec examples.
func New(cfg Config) (*Double, error) {
	cat, err := gen.Catalogue()
	if err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Func(func() time.Time { return seed.At })
	}
	d := &Double{base: cfg.Clock}
	clk := clock.Func(d.now)
	if cfg.IDs == nil {
		cfg.IDs = ids.NewGenerator(clk.Now, &counter{})
	}
	d.gw, err = fakegw.New(clk.Now)
	if err != nil {
		return nil, err
	}
	// The gateway hands each stored op to subscribers as a sync.op event, after the emitting request ended
	// (the bus never delivers inside the emitting transaction): see ServeHTTP.
	d.gw.Deliver = func(_ context.Context, ev events.Envelope) error {
		d.mu.Lock()
		d.pending = append(d.pending, ev)
		d.mu.Unlock()
		return nil
	}
	d.gw.SetDown(cfg.NoEmitter)
	store := mem.New()
	log := slog.New(slog.DiscardHandler)
	d.Store = store
	d.ids = seed.NewIDs(cfg.IDs)
	d.Svc = service.New(store, service.Options{Clock: clk, IDs: d.ids, Log: log, Emitter: d.gw})
	guard := &auth.Guard{Catalogue: cat, Tenant: cfg.Tenant, Services: cfg.Services, Audit: store, Log: log}
	d.h = api.New(api.Config{Service: d.Svc, Guard: guard, Clock: clk, Log: log})
	return d, nil
}

type counter struct{ n byte }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		c.n++
		p[i] = c.n
	}
	return len(p), nil
}

// SetGatewayDown makes the sync gateway unreachable (true) or reachable again (false). Ops queued while it is
// down are sent, in order, by the next request or Settle.
func (d *Double) SetGatewayDown(down bool) { d.gw.SetDown(down) }

// Settle runs the emit worker and the bus once: queued ops go to the gateway in order, and every op the gateway
// stored is delivered to catalog's projection as a sync.op event. A request does this itself on return.
func (d *Double) Settle(ctx context.Context) {
	for _, q := range d.Store.PendingEmits() {
		if _, err := d.gw.Emit(ctx, q.Tenant, []emit.Op{q.Op}); err != nil {
			if emit.IsTransient(err) {
				break // the gateway is down: the rest waits, in order
			}
			slog.Error("double: queued emit refused", "error", err)
			continue
		}
		d.Store.MarkEmitted(q.Tenant, q.Op.Key)
	}
	d.mu.Lock()
	p := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, ev := range p {
		if err := d.Svc.ApplyEventTx(ctx, ev); err != nil {
			slog.Error("double: delivery failed", "error", err)
		}
	}
}

// Emitted returns the ops the gateway fake stored, in order.
func (d *Double) Emitted() []Emitted { return d.gw.Stored() }

// ApplyOp delivers a device op for its tenant, as the bus would.
func (d *Double) ApplyOp(ctx context.Context, op *gsync.Op) error { return d.Svc.ApplyOpTx(ctx, op) }

// SeedWorld empties the double and loads the named example world of the OpenAPI document for the example
// organisation (contracttest Options.SeedWorld).
func (d *Double) SeedWorld(ctx context.Context, world string) error {
	d.Store.Reset()
	d.gw.Reset()
	d.mu.Lock()
	d.pending, d.offset = nil, 0
	d.mu.Unlock()
	if err := seed.Example.Apply(ctx, d.Svc, world); err != nil {
		return err
	}
	d.ids.Next(seed.Example, world)
	d.mu.Lock()
	d.offset = seed.ClockAdvance(world)
	d.mu.Unlock()
	return nil
}

// SeedOrganisation adds another organisation's base world without resetting (isolation tests).
func (d *Double) SeedOrganisation(ctx context.Context, w seed.World) error {
	return w.Apply(ctx, d.Svc, seed.Base)
}

// Inject adds a failure for matching requests; it returns a function that removes it.
func (d *Double) Inject(f Fault) (remove func()) {
	d.mu.Lock()
	d.fault = append(d.fault, f)
	i := len(d.fault) - 1
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.fault[i].Match = nil
		d.mu.Unlock()
	}
}

// ServeHTTP implements http.Handler.
func (d *Double) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	var hit *Fault
	for i := range d.fault {
		if d.fault[i].Match != nil && d.fault[i].Match(r) {
			f := d.fault[i]
			hit = &f
			break
		}
	}
	d.mu.Unlock()
	if hit != nil {
		if hit.Hang {
			<-r.Context().Done()
			return
		}
		if hit.Delay > 0 {
			select {
			case <-time.After(hit.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if hit.Status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(hit.Status)
			_, _ = fmt.Fprintf(w, `{"type":"urn:hostry:problem:injected","title":"Injected failure","status":%d,"code":"injected"}`, hit.Status)
			return
		}
	}
	d.h.ServeHTTP(w, r)
	d.Settle(context.WithoutCancel(r.Context()))
}

// Server starts the double on an httptest server.
func (d *Double) Server() *httptest.Server { return httptest.NewServer(d) }

// Load adds an organisation's named example world without resetting what is there (isolation tests).
func (d *Double) Load(ctx context.Context, w seed.World, world string) error {
	return w.Apply(ctx, d.Svc, world)
}
