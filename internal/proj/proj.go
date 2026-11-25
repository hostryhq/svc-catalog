// Package proj is catalog's projection engine: the conflict policy of the entities catalog owns (ADR-0002 S6,
// api-specs entities/restaurant, owner catalog) applied to the ops that arrive from the sync gateway.
//
// A row is a pure function of the set of ops applied to it and of the void set, so two nodes that hold the
// same ops hold the same row whatever the arrival order. Every mutation is kept as an effect (the candidates
// of S6); the visible row is derived from the effects, and a void removes the effects of the refused op so a
// later legitimate edit is never overwritten (the same semantics as the gateway's own state, svc-sync
// engine.State, which go-common/sync will host).
//
// Conflict policy of every catalog entity: last writer wins per field by the hybrid logical clock stamp
// (hlc, origin, seq, index in op). Fields marked review (price and tax class) additionally carry `prev`, the
// stamp of the value the editor saw; a write whose prev is not the stamp just before it was written without
// seeing it, which is the concurrent edit the detector reports (RM-M4).
package proj

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hostryhq/svc-catalog/internal/model"
)

// Mutation kinds of an op (ADR-0002 S4). Catalog entities use insert, set, del and undel.
const (
	KInsert = "insert"
	KSet    = "set"
	KDel    = "del"
	KUndel  = "undel"
)

// Intent names the projection treats specially.
const (
	IntentVoid = "sync.void"
	EntityVoid = "sync_void"
)

// Entity is the declaration of one catalog entity: the stream it lives in and its review fields.
type Entity struct {
	Stream string
	// Review lists the lww+review fields.
	Review map[string]bool
	// PriceFields are the review fields that hold money, reported as price_conflict.
	PriceFields map[string]bool
}

func entity(stream string, review ...string) Entity {
	e := Entity{Stream: stream, Review: map[string]bool{}, PriceFields: map[string]bool{}}
	for _, f := range review {
		e.Review[f] = true
		if strings.HasSuffix(f, "_paise") {
			e.PriceFields[f] = true
		}
	}
	return e
}

// Entities are the 17 entities owned by catalog (api-specs entities/README.md ownership table).
var Entities = map[string]Entity{
	"category":             entity(model.StreamBrand, "default_tax_class"),
	"item":                 entity(model.StreamBrand, "tax_class"),
	"item_variant":         entity(model.StreamBrand),
	"modifier_group":       entity(model.StreamBrand),
	"modifier":             entity(model.StreamBrand, "price_delta_paise"),
	"item_modifier_group":  entity(model.StreamBrand),
	"price_entry":          entity(model.StreamBrand, "price_paise"),
	"reason_code":          entity(model.StreamBrand),
	"item_outlet":          entity(model.StreamOutlet, "outlet_price_paise"),
	"station":              entity(model.StreamOutlet),
	"floor":                entity(model.StreamOutlet),
	"dining_table":         entity(model.StreamOutlet),
	"printer":              entity(model.StreamOutlet),
	"print_route":          entity(model.StreamOutlet),
	"receipt_profile":      entity(model.StreamOutlet),
	"outlet_power_profile": entity(model.StreamOutlet),
	"outlet_settings":      entity(model.StreamOutlet),
}

// Mut is one generic mutation inside an op.
type Mut struct {
	E    string
	ID   string
	K    string
	F    map[string]any
	Prev map[string]string
}

// Ref is the identity of an op.
type Ref struct {
	Origin string
	Stream string
	Seq    int64
}

// Key is the stable text form of the identity.
func (r Ref) Key() string { return r.Origin + "/" + r.Stream + "/" + strconv.FormatInt(r.Seq, 10) }

// Op is a decoded op body.
type Op struct {
	Ref
	HLC    uint64
	Intent string
	Muts   []Mut
}

// ErrBody is returned for a body that is not a valid op.
var ErrBody = errors.New("proj: malformed op body")

type bodyJSON struct {
	Origin string `json:"origin"`
	Stream string `json:"stream"`
	Seq    int64  `json:"seq"`
	HLC    string `json:"hlc"`
	Intent string `json:"intent"`
	Muts   []struct {
		E    string            `json:"e"`
		ID   string            `json:"id"`
		K    string            `json:"k"`
		F    map[string]any    `json:"f"`
		Prev map[string]string `json:"prev"`
	} `json:"muts"`
}

// ParseBody decodes the canonical JSON text of an op body (svc-sync contract/README.md). Field values are
// strings, booleans and integers; a number that is not an integer is refused.
func ParseBody(text string) (Op, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var b bodyJSON
	if err := dec.Decode(&b); err != nil {
		return Op{}, fmt.Errorf("%w: %v", ErrBody, err)
	}
	if b.Origin == "" || b.Stream == "" || b.Seq < 1 || b.Intent == "" {
		return Op{}, fmt.Errorf("%w: identity fields missing", ErrBody)
	}
	h, err := ParseHLC(b.HLC)
	if err != nil {
		return Op{}, fmt.Errorf("%w: %v", ErrBody, err)
	}
	op := Op{Ref: Ref{Origin: b.Origin, Stream: b.Stream, Seq: b.Seq}, HLC: h, Intent: b.Intent}
	for _, m := range b.Muts {
		mm := Mut{E: m.E, ID: m.ID, K: m.K, Prev: m.Prev, F: map[string]any{}}
		for k, v := range m.F {
			if n, ok := v.(json.Number); ok {
				x, err := strconv.ParseInt(string(n), 10, 64)
				if err != nil {
					return Op{}, fmt.Errorf("%w: field %s is not an integer", ErrBody, k)
				}
				v = x
			}
			mm.F[k] = v
		}
		op.Muts = append(op.Muts, mm)
	}
	return op, nil
}

// ParseHLC parses the 16-hex-character hybrid logical clock.
func ParseHLC(s string) (uint64, error) {
	if len(s) != 16 {
		return 0, errors.New("hlc: want 16 hex characters")
	}
	return strconv.ParseUint(s, 16, 64)
}

// FormatHLC is the 16-hex-character form.
func FormatHLC(h uint64) string { return fmt.Sprintf("%016x", h) }

// StreamScope splits a stream name ("brand:<id>", "outlet:<id>", "tenant:<id>").
func StreamScope(stream string) (kind, id string, ok bool) {
	kind, id, ok = strings.Cut(stream, ":")
	return kind, id, ok && id != ""
}

// Stamp orders writes: hlc, then origin, seq and index in the op.
type Stamp struct {
	HLC    uint64
	Origin string
	Seq    int64
	Idx    int
}

// Less reports whether a sorts before b.
func (a Stamp) Less(b Stamp) bool {
	if a.HLC != b.HLC {
		return a.HLC < b.HLC
	}
	if a.Origin != b.Origin {
		return a.Origin < b.Origin
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	return a.Idx < b.Idx
}

// Effect is one mutation of one row, kept until its op is voided.
type Effect struct {
	Op   string
	St   Stamp
	Kind string
	F    map[string]any
	Prev map[string]string
}

// Derived is the visible state of a row.
type Derived struct {
	Exists bool
	Gone   bool
	Fields map[string]any
	// Stamps is the hlc (hex) of the winning write of each field.
	Stamps map[string]string
}

// Derive computes the visible row from its effects. A row exists once an insert was applied; the latest
// insert, del or undel decides whether it is tombstoned.
func Derive(effects []Effect) Derived {
	d := Derived{Fields: map[string]any{}, Stamps: map[string]string{}}
	var ins, tomb []Effect
	for _, e := range effects {
		switch e.Kind {
		case KInsert:
			ins = append(ins, e)
			tomb = append(tomb, e)
		case KDel, KUndel:
			tomb = append(tomb, e)
		}
	}
	if len(ins) == 0 {
		return d
	}
	best := map[string]Effect{}
	for _, e := range effects {
		if e.Kind != KInsert && e.Kind != KSet {
			continue
		}
		for f := range e.F {
			if cur, ok := best[f]; !ok || cur.St.Less(e.St) {
				best[f] = e
			}
		}
	}
	for f, e := range best {
		d.Fields[f] = e.F[f]
		d.Stamps[f] = FormatHLC(e.St.HLC)
	}
	d.Exists = true
	top := tomb[0]
	for _, e := range tomb[1:] {
		if top.St.Less(e.St) {
			top = e
		}
	}
	d.Gone = top.Kind == KDel
	return d
}

// Conflict is one concurrent edit of a review field: Later was written without seeing Earlier, so the later
// stamp wins and the earlier value is the one retained as lost (RM-M4).
type Conflict struct {
	Field      string
	Earlier    Effect
	Later      Effect
	Superseded bool // a later edit saw everything before it
}

// Conflicts finds the concurrent edits of the review fields of the entity. Output is sorted by field and
// stamp and depends only on the effects.
func Conflicts(entityName string, effects []Effect) []Conflict {
	def, ok := Entities[entityName]
	if !ok {
		return nil
	}
	fields := make([]string, 0, len(def.Review))
	for f := range def.Review {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	var out []Conflict
	for _, f := range fields {
		var cands []Effect
		for _, e := range effects {
			if e.Kind != KSet && e.Kind != KInsert {
				continue
			}
			if _, ok := e.F[f]; ok {
				cands = append(cands, e)
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].St.Less(cands[j].St) })
		saw := func(i int) bool { return cands[i].Kind == KSet && cands[i].Prev[f] == FormatHLC(cands[i-1].St.HLC) }
		for i := 1; i < len(cands); i++ {
			if cands[i].Kind != KSet || saw(i) {
				continue
			}
			c := Conflict{Field: f, Earlier: cands[i-1], Later: cands[i]}
			for j := i + 1; j < len(cands); j++ {
				if saw(j) {
					c.Superseded = true
					break
				}
			}
			out = append(out, c)
		}
	}
	return out
}
