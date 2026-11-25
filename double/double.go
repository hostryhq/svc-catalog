// Package double is the in-memory test double of the catalog service. It runs the real API layer and service
// over an in-memory repository, so it implements the whole public contract (scoping, idempotency, the
// projection of synced ops, menu import) with deterministic ids and time. Other services' tests import it
// instead of the real service. It is verified by the shared contract suite (api-specs/contracttest).
//
// Cloud-side writes (tax class, menu import) go to a loopback emitter that records each op and feeds it back
// through the projection, as the sync gateway and the bus would; ApplyOp lets a test deliver a device op.
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
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/api"
	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/proj"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/repository/mem"
	"github.com/hostryhq/svc-catalog/service"
)

// Config configures the double. Tenant is required; the rest has defaults.
type Config struct {
	Tenant *auth.Verifier // tenant-realm tokens (authtest.NewTenantIssuer(...).Verifier())
	// Clock defaults to a fixed instant, the instant of the spec examples.
	Clock clock.Clock
	// IDs defaults to a generator driven by Clock with a counter, so ids are deterministic.
	IDs *ids.Generator
	// NoEmitter makes cloud-side writes fail with 503 (a gateway that is down).
	NoEmitter bool
}

// Fault is an injected failure for matching requests.
type Fault struct {
	Match  func(*http.Request) bool
	Status int           // respond with this status and a problem body (0: pass through)
	Delay  time.Duration // sleep before answering
	Hang   bool          // never answer until the request is cancelled (a timeout for the caller)
}

// Emitted is one op the loopback recorded.
type Emitted struct {
	Tenant ids.ID
	service.Emission
	OpID string
}

// Double is the running double.
type Double struct {
	Store *mem.Store
	Svc   *service.Service
	h     http.Handler
	clk   clock.Clock

	mu      sync.Mutex
	fault   []Fault
	emitted []Emitted
	pending []pendingOp
	byKey   map[string]string
	seqs    map[string]int64
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
	if cfg.IDs == nil {
		cfg.IDs = ids.NewGenerator(cfg.Clock.Now, &counter{})
	}
	store := mem.New()
	log := slog.New(slog.DiscardHandler)
	svc := service.New(store, service.Options{Clock: cfg.Clock, IDs: cfg.IDs, Log: log})
	d := &Double{Store: store, Svc: svc, clk: cfg.Clock, byKey: map[string]string{}, seqs: map[string]int64{}}
	if !cfg.NoEmitter {
		svc.SetEmitter(d)
	}
	guard := &auth.Guard{Catalogue: cat, Tenant: cfg.Tenant, Audit: store, Log: log}
	d.h = api.New(api.Config{Service: svc, Guard: guard, Clock: cfg.Clock, Log: log})
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

// Emit implements service.Emitter: the loopback gateway. A repeated key returns the first op.
func (d *Double) Emit(ctx context.Context, tenant ids.ID, e service.Emission) (service.Emitted, error) {
	d.mu.Lock()
	k := tenant.String() + "|" + e.Key
	if e.Key != "" {
		if id, ok := d.byKey[k]; ok {
			d.mu.Unlock()
			return service.Emitted{OpID: id}, nil
		}
	}
	d.seqs[tenant.String()+e.Stream]++
	n := d.seqs[tenant.String()+e.Stream]
	opID := fmt.Sprintf("cloud-1/%s/%d", e.Stream, n)
	if e.Key != "" {
		d.byKey[k] = opID
	}
	d.emitted = append(d.emitted, Emitted{Tenant: tenant, Emission: e, OpID: opID})
	op := proj.Op{Ref: proj.Ref{Origin: "cloud-1", Stream: e.Stream, Seq: n}, HLC: uint64(d.clk.Now().UnixMilli())<<16 + uint64(n), Intent: e.Intent, Muts: e.Muts}
	d.pending = append(d.pending, pendingOp{tenant, op})
	d.mu.Unlock()
	return service.Emitted{OpID: opID}, nil
}

type pendingOp struct {
	tenant ids.ID
	op     proj.Op
}

// drain delivers the ops emitted during a request, after its transaction ended (the bus delivers an op after
// the gateway stored it, never inside the emitting request).
func (d *Double) drain(ctx context.Context) {
	d.mu.Lock()
	p := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, x := range p {
		if err := d.Svc.ApplyOpTx(ctx, x.tenant, x.op); err != nil {
			slog.Error("double: loopback delivery failed", "error", err)
		}
	}
}

// Emitted returns the ops the loopback recorded, in order.
func (d *Double) Emitted() []Emitted {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Emitted(nil), d.emitted...)
}

// ApplyOp delivers a device op for tenant, as the bus would.
func (d *Double) ApplyOp(ctx context.Context, tenant ids.ID, op proj.Op) error {
	return d.Svc.ApplyOpTx(ctx, tenant, op)
}

// SeedWorld empties the double and loads the named example world of the OpenAPI document for the example
// organisation (contracttest Options.SeedWorld).
func (d *Double) SeedWorld(ctx context.Context, world string) error {
	d.Store.Reset()
	d.mu.Lock()
	d.emitted, d.byKey, d.seqs = nil, map[string]string{}, map[string]int64{}
	d.mu.Unlock()
	return seed.Example.Apply(ctx, d.Svc, world)
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
	d.drain(context.WithoutCancel(r.Context()))
}

// Server starts the double on an httptest server.
func (d *Double) Server() *httptest.Server { return httptest.NewServer(d) }
