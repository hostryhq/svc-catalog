// Package service holds the business rules of the catalog service. It has no HTTP and no SQL: it depends on
// repository interfaces, takes the tenant from the tenancy context and the caller's outlet scope from go-auth,
// projects the synced catalog entities from the ops the sync gateway publishes, and writes cloud-side changes
// (tax class, menu import) back through the gateway emit API as ops (ADR-0014 R1).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/repository"
)

// Permissions catalog checks itself (the guard already required the operation's declared permission).
const (
	PermMenuRead     = "menu.read"
	PermSettingsRead = "settings.read"
	PermExceptions   = "exceptions.view"
	PermTaxClass     = "menu.tax_class.edit"
	PermImport       = "menu.import"
)

// DefaultTaxClasses are the Phase 2 tax classes (api-specs TaxClass). The rule set in force is billing's data;
// until billing exposes the classes of the published set to catalog, the accepted classes are configuration
// (see docs/notes.md).
var DefaultTaxClasses = []string{"fnb_standard", "fnb_non_gst", "exempt"}

// OpID is the identity of a stored op as the API reports it: "<origin>/<stream>/<seq>".
func OpID(r emit.Result) string {
	return r.Origin + "/" + r.Stream + "/" + strconv.FormatInt(r.Seq, 10)
}

// IDSource mints the ids of rows catalog creates. *ids.Generator is one; the double and the example-world tests
// use a source that hands out the ids of the spec examples first (seed.IDs).
type IDSource interface{ New() ids.ID }

// Service is the catalog business layer.
type Service struct {
	repo       repository.Repository
	clock      clock.Clock
	gen        IDSource
	log        *slog.Logger
	emitter    emit.Emitter
	reg        *gsync.Registry
	parser     gsync.Parser
	taxClasses []string
	// importBatch is how many items go into one emitted op.
	importBatch int
}

// Options configure a Service. Zero values pick production defaults.
type Options struct {
	Clock clock.Clock
	IDs   IDSource // default: a UUIDv7 generator on the clock
	Log   *slog.Logger
	// Emitter is the gateway emit client for interactive edits (nil: they answer 503 emit.unavailable). Queued
	// emissions (menu import) go through emit_queue and need no emitter here.
	Emitter    emit.Emitter
	Registry   *gsync.Registry // default: the embedded entity registry
	TaxClasses []string
	// ImportBatch is the number of items per emitted op (default 50).
	ImportBatch int
}

// New builds a Service over repo.
func New(repo repository.Repository, o Options) *Service {
	s := &Service{repo: repo, clock: o.Clock, gen: o.IDs, log: o.Log, emitter: o.Emitter, reg: o.Registry, taxClasses: o.TaxClasses, importBatch: o.ImportBatch}
	if s.reg == nil {
		s.reg = gen.MustRegistry()
	}
	if s.clock == nil {
		s.clock = clock.System
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(s.clock.Now, nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if len(s.taxClasses) == 0 {
		s.taxClasses = DefaultTaxClasses
	}
	if s.importBatch <= 0 {
		s.importBatch = 50
	}
	return s
}

// SetEmitter wires the gateway emit client after construction (the double builds its gateway from the service).
func (s *Service) SetEmitter(e emit.Emitter) { s.emitter = e }

// Idem identifies one idempotent request: the operation, the client's key and the request fingerprint.
type Idem struct{ Op, Key, Fingerprint string }

type idemKey struct{}

// WithIdem marks ctx as carrying an Idempotency-Key request; the next transaction replays or records it.
func WithIdem(ctx context.Context, i Idem) context.Context {
	return context.WithValue(ctx, idemKey{}, i)
}

// IdemFrom returns the idempotency identity of the request in ctx.
func IdemFrom(ctx context.Context) (Idem, bool) { i, ok := ctx.Value(idemKey{}).(Idem); return i, ok }

// run executes fn in one transaction for the tenant in ctx. With an Idempotency-Key in ctx it first claims the
// key in the same transaction: a replay returns the stored result and runs nothing, so the effect and the key
// commit together. Errors are not stored.
func run[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		im, ok := IdemFrom(ctx)
		if !ok {
			v, err := fn(ctx, tx)
			out = v
			return err
		}
		fp, stored, found, err := tx.IdemGet(ctx, im.Op, im.Key)
		if err != nil {
			return err
		}
		if found {
			if fp != im.Fingerprint {
				return idempotency.ErrMismatch
			}
			return json.Unmarshal(stored, &out)
		}
		v, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = v
		return tx.IdemPut(ctx, im.Op, im.Key, im.Fingerprint, b, s.clock.Now())
	})
	var zero T
	if err != nil {
		return zero, err
	}
	return out, nil
}

// read runs fn in a transaction without idempotency.
func read[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		v, err := fn(ctx, tx)
		out = v
		return err
	})
	return out, err
}

func tenantOf(ctx context.Context) (ids.ID, error) {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return "", err
	}
	return tc.TenantID, nil
}

// actor is the member, device or service performing the action (audit).
func actor(ctx context.Context) string {
	if tc, ok := tenancy.From(ctx); ok {
		return tc.ActorID
	}
	return ""
}

func repoErr(err error) error {
	if errors.Is(err, model.ErrRepoNotFound) {
		return model.ErrNotFound
	}
	return err
}

func (s *Service) newID() ids.ID { return s.gen.New() }

func errNotPreviewed() error {
	return apperr.New(apperr.Conflict, "import_not_previewed", "the import is not awaiting its final step")
}

// Scope resolves the outlets a request may touch. The caller's scope for perm comes from go-auth; an
// X-Outlet-Id narrows it to that outlet. An outlet that is outside the caller's scope, unknown, or another
// tenant's answers the same 404 (ADR-0003 A1.4 item 4). It returns whether an outlet was named.
func (s *Service) scope(ctx context.Context, tx repository.Tx, perm string, outlet string) (auth.OutletScope, bool, error) {
	sc, err := auth.ScopeFor(ctx, perm)
	if err != nil {
		return auth.OutletScope{}, false, err
	}
	if outlet == "" {
		return sc, false, nil
	}
	id, err := ids.Parse(outlet)
	if err != nil || !sc.Contains(outlet) {
		return auth.OutletScope{}, false, model.ErrNotFound
	}
	if _, err := tx.GetOutlet(ctx, id); err != nil {
		return auth.OutletScope{}, false, repoErr(err)
	}
	return auth.OnlyOutlets(outlet), true, nil
}

// brandsFor is the set of brands whose brand-wide data the scope reaches. An organisation-wide scope with no
// named outlet reaches every brand of the tenant, including brands whose outlets catalog has not heard of yet.
func brandsFor(ctx context.Context, tx repository.Tx, sc auth.OutletScope, named bool) (repository.BrandSet, error) {
	if sc.All() && !named {
		return repository.BrandSet{All: true}, nil
	}
	b, err := tx.BrandsOf(ctx, sc)
	return repository.BrandSet{IDs: b}, err
}

// paginate trims rows fetched with limit+1 and reports whether another page exists.
func paginate[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// Page is one page of results with the id to continue after ("" when it is the last page).
type Page[T any] struct {
	Items []T
	Next  string
}

func pageOf[T any](rows []T, limit int, idOf func(T) string) Page[T] {
	items, more := paginate(rows, limit)
	p := Page[T]{Items: items}
	if more && len(items) > 0 {
		p.Next = idOf(items[len(items)-1])
	}
	return p
}

func (s *Service) validTaxClass(field, class string) error {
	if !slices.Contains(s.taxClasses, class) {
		return model.Invalid(field, "tax_class_unknown", "the tax class is not in the rule set in force")
	}
	return nil
}

func (s *Service) publish(ctx context.Context, tx repository.Tx, e events.Envelope) error {
	return tx.Publish(ctx, e)
}

// ---- small helpers shared by the files of this package ----

func itoa(n int) string { return strconv.Itoa(n) }

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func now(s *Service) time.Time { return s.clock.Now().UTC() }
