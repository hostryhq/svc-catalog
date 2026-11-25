// Package fakegw is an in-process fake of the sync gateway's emit operation, POST /internal/v1/emit of
// openapi/sync/v1 (ADR-0014 A2.2). The real endpoint is not built yet; catalog is tested against this fake
// through the real go-common/emit client and emit_queue worker.
//
// It does what the contract says the gateway does: checks the caller owns the intent, that the intent is
// cloud-origin, that every mutation is of an entity and kind the intent lists, the size limits and the batch
// size; stamps origin cloud-1, a seq per (tenant, stream) from 1 and an hlc; stores each op once per
// (tenant, caller, key) (same key and content: the original identity with replayed true; same key, other
// content: 422 idempotency.mismatch); and, like the bus, hands each stored op to Deliver as a sync.op event.
// Nothing is stored when any op of a batch is refused. Failure injection: Down makes it answer 503.
//
// The intent table is api-specs sync/intents/cloud.yaml (embedded by scripts/gen.sh), so a catalog emit the
// registry would refuse is refused here.
package fakegw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-catalog/internal/gen"
)

// Origin is the cloud origin the fake stamps (cloud-<epoch>).
const Origin = "cloud-1"

// Caller is the service the fake authenticates callers as.
const Caller = "catalog"

// Sealed is a stored op as the bus would carry it.
type Sealed struct {
	Tenant  ids.ID
	N       int64  // position in the gateway's storage order, from 1 (the event id)
	Key     string // the caller's idempotency key
	Origin  string
	Stream  string
	Seq     int64
	HLC     string
	Intent  string
	IntentV int
	Actor   string
	At      time.Time
	Muts    []gsync.Mut
}

// Gateway is the fake. The zero value is not usable; use New.
type Gateway struct {
	// Deliver receives every stored op (nil: none). It runs after the op is stored and the fake's lock is
	// released, in storage order, on the goroutine that emitted.
	Deliver func(ctx context.Context, ev events.Envelope) error

	clock   func() time.Time
	intents map[string]gen.CloudIntent
	mu      sync.Mutex
	down    bool
	seqs    map[string]int64
	byKey   map[string]stored
	log     []Sealed
	ctr     int64
}

type stored struct {
	content string
	res     emit.Result
}

// New builds a gateway whose op times come from clock.
func New(clock func() time.Time) (*Gateway, error) {
	in, err := gen.CloudIntents()
	if err != nil {
		return nil, err
	}
	return &Gateway{clock: clock, intents: in, seqs: map[string]int64{}, byKey: map[string]stored{}}, nil
}

// SetDown makes the gateway unavailable (503 emit.unavailable) or available again.
func (g *Gateway) SetDown(down bool) {
	g.mu.Lock()
	g.down = down
	g.mu.Unlock()
}

// Stored returns the stored ops in order.
func (g *Gateway) Stored() []Sealed {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.log)
}

// Reset forgets everything stored.
func (g *Gateway) Reset() {
	g.mu.Lock()
	g.seqs, g.byKey, g.log, g.ctr = map[string]int64{}, map[string]stored{}, nil, 0
	g.mu.Unlock()
}

// Emit implements emit.Emitter with the semantics of the endpoint, without HTTP (the double).
func (g *Gateway) Emit(ctx context.Context, tenant ids.ID, ops []emit.Op) ([]emit.Result, error) {
	res, sealed, err := g.store(tenant, ops)
	var rf *refusal
	if errors.As(err, &rf) {
		return nil, &emit.RejectedError{Status: rf.status, Code: rf.code} // as the client reports a 422
	}
	if err != nil {
		return nil, err
	}
	for _, s := range sealed {
		if g.Deliver != nil {
			if err := g.Deliver(ctx, Event(s)); err != nil {
				return nil, fmt.Errorf("fakegw: deliver: %w", err)
			}
		}
	}
	return res, nil
}

// refusal is a gateway refusal.
type refusal struct {
	status int
	code   string
}

func (r *refusal) Error() string { return fmt.Sprintf("fakegw: %d %s", r.status, r.code) }

func (g *Gateway) store(tenant ids.ID, ops []emit.Op) ([]emit.Result, []Sealed, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return nil, nil, emit.NewUnavailable(errors.New("fakegw down"), time.Second)
	}
	if err := gsync.CheckBatch(len(ops)); err != nil {
		return nil, nil, &refusal{http.StatusUnprocessableEntity, "emit.batch_size"}
	}
	for _, o := range ops {
		if err := g.check(o); err != nil {
			return nil, nil, err
		}
	}
	res := make([]emit.Result, len(ops))
	var sealed []Sealed
	pendingSeq := map[string]int64{}
	pendingKey := map[string]stored{}
	for i, o := range ops {
		k := tenant.String() + "|" + Caller + "|" + o.Key
		c := content(o)
		prev, ok := g.byKey[k]
		if p, dup := pendingKey[k]; dup {
			prev, ok = p, true
		}
		if ok {
			if prev.content != c {
				return nil, nil, &refusal{http.StatusUnprocessableEntity, "idempotency.mismatch"}
			}
			r := prev.res
			r.Replayed = true
			res[i] = r
			continue
		}
		sk := tenant.String() + "|" + o.Stream
		seq := g.seqs[sk] + pendingSeq[sk] + 1
		pendingSeq[sk]++
		now := g.clock().UTC()
		n := g.ctr + int64(len(sealed)) + 1
		hlc := fmt.Sprintf("%016x", uint64(now.UnixMilli())<<16+uint64(n))
		res[i] = emit.Result{Key: o.Key, Origin: Origin, Stream: o.Stream, Seq: seq}
		pendingKey[k] = stored{content: c, res: res[i]}
		sealed = append(sealed, Sealed{Tenant: tenant, N: n, Key: o.Key, Origin: Origin, Stream: o.Stream, Seq: seq, HLC: hlc, Intent: o.Intent, IntentV: o.IntentV, Actor: o.Actor, At: now, Muts: o.Muts})
	}
	for k, v := range pendingKey {
		g.byKey[k] = v
	}
	for sk, n := range pendingSeq {
		g.seqs[sk] += n
	}
	g.ctr += int64(len(sealed))
	g.log = append(g.log, sealed...)
	return res, sealed, nil
}

// check is the gateway's per-op validation.
func (g *Gateway) check(o emit.Op) error {
	if err := o.Validate(); err != nil {
		if strings.Contains(err.Error(), "op.too_large") {
			return &refusal{http.StatusUnprocessableEntity, "op.too_large"}
		}
		return &refusal{http.StatusUnprocessableEntity, "emit.invalid"}
	}
	in, ok := g.intents[o.Intent]
	switch {
	case !ok:
		return &refusal{http.StatusUnprocessableEntity, "emit.intent_unknown"}
	case !in.Cloud:
		return &refusal{http.StatusUnprocessableEntity, "emit.intent_not_cloud"}
	case in.Owner != Caller:
		return &refusal{http.StatusUnprocessableEntity, "emit.caller_not_owner"}
	case o.AuthorisedBy != nil && !slices.Contains(in.AuthorisedOnlineBy, *o.AuthorisedBy):
		return &refusal{http.StatusUnprocessableEntity, "emit.invalid"} // not a gateway check; catches a caller that names a permission the intent does not list
	case !slices.Contains(in.Streams, streamKind(o.Stream)):
		return &refusal{http.StatusUnprocessableEntity, "emit.stream_not_in_tenant"}
	}
	for _, m := range o.Muts {
		if !slices.Contains(in.Entities[m.E], m.K) {
			return &refusal{http.StatusUnprocessableEntity, "emit.entity_not_allowed"}
		}
	}
	return nil
}

func streamKind(stream string) string { k, _, _ := strings.Cut(stream, ":"); return k }

func content(o emit.Op) string {
	b, _ := json.Marshal(struct {
		Stream, Intent string
		IntentV        int
		Muts           []gsync.Mut
		PII            map[string]map[string]map[string]any
		Actor          string
		AuthorisedBy   *string
		Approval       string
	}{o.Stream, o.Intent, o.IntentV, o.Muts, o.PII, o.Actor, o.AuthorisedBy, o.Approval})
	return string(b)
}

// Event builds the sync.op.<namespace> bus event of a stored op (events/sync/op.v1.yaml): the exact body bytes,
// their hash, the envelope key and key_seq, outlet_id for an outlet stream.
func Event(s Sealed) events.Envelope {
	body := map[string]any{"v": 1, "origin": s.Origin, "stream": s.Stream, "seq": s.Seq, "hlc": s.HLC, "clk": "sync", "tenant_id": s.Tenant.String(),
		"actor": s.Actor, "intent": s.Intent, "intent_v": s.IntentV, "at": s.At.UTC().Format("2006-01-02T15:04:05.000Z"), "ph": gsync.Genesis(s.Origin, s.Stream),
		"muts": s.Muts}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	canon, err := gsync.Canonicalize(raw)
	if err != nil {
		panic(err)
	}
	text := string(canon)
	ns, _, _ := strings.Cut(s.Intent, ".")
	data, _ := json.Marshal(map[string]any{"origin": s.Origin, "stream": s.Stream, "seq": s.Seq, "h": gsync.Hash(text), "intent": s.Intent,
		"intent_v": s.IntentV, "status": "accepted", "marks": []string{}, "body": text})
	env := events.Envelope{ID: ids.ID(fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-%012x", uint64(s.N)&0xffffffffffff)), Type: "sync.op." + ns, V: 1, Source: "sync", TenantID: s.Tenant,
		Key: s.Stream + "|" + s.Origin, KeySeq: s.Seq, OccurredAt: s.At.UTC(), Data: data}
	if kind, id, _ := strings.Cut(s.Stream, ":"); kind == "outlet" {
		env.OutletID = ids.ID(id)
	}
	return env
}

// ServeHTTP is POST /internal/v1/emit. It requires a service token header (its value is not verified here) and
// answers as the contract does: 200 results, 422 problems with the gateway's codes, 503 emit.unavailable with
// Retry-After.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != emit.Path {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(emit.ServiceTokenHeader) == "" {
		problem(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req struct {
		TenantID ids.ID    `json:"tenant_id"`
		Ops      []emit.Op `json:"ops"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil || req.TenantID == "" {
		problem(w, http.StatusUnprocessableEntity, "emit.invalid")
		return
	}
	for i := range req.Ops { // the gateway reads integers as integers (the body carries integers only)
		for j := range req.Ops[i].Muts {
			normalise(req.Ops[i].Muts[j].F)
		}
	}
	res, err := g.Emit(r.Context(), req.TenantID, req.Ops)
	var rej *emit.RejectedError
	switch {
	case errors.As(err, &rej):
		problem(w, rej.Status, rej.Code)
	case err != nil:
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, "emit.unavailable")
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ops": res})
	}
}

// normalise turns the whole-number float64 values json.Unmarshal produced back into int64.
func normalise(f map[string]any) {
	for k, v := range f {
		if x, ok := v.(float64); ok && x == float64(int64(x)) {
			f[k] = int64(x)
		}
	}
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:hostry:problem:" + code, "title": code, "status": status, "code": code})
}
