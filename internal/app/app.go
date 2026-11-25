// Package app wires the catalog service: repository, service, go-auth guard, HTTP handler and the bus
// consumer handler. The process (cmd/catalog) and the integration tests build it the same way.
package app

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/health"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"

	"github.com/hostryhq/svc-catalog/api"
	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/repository"
	"github.com/hostryhq/svc-catalog/repository/pg"
	"github.com/hostryhq/svc-catalog/service"

	"context"
)

// Options are the dependencies of a running service.
type Options struct {
	AppDB *sql.DB // runtime pool as catalog_app

	Tenant   *auth.Verifier
	Services *auth.ServiceVerifier // optional: without it support sessions are refused

	Clock     clock.Clock // entity timestamps and ids
	AuthClock clock.Clock // token checks; defaults to Clock
	IDs       service.IDSource
	Log       *slog.Logger
	// Emitter is the gateway emit client for interactive cloud-side edits (nil: they answer 503 emit.unavailable).
	Emitter emit.Emitter
	// TaxClasses are the tax classes an owner may choose (default: the Phase 2 classes).
	TaxClasses []string
}

// App is the wired service.
type App struct {
	Handler http.Handler
	Service *service.Service
	Repo    repository.Repository
	Health  *health.Handler
	// Consume is the inbox handler for the bus events catalog subscribes to (sync ops, tenancy outlet events).
	Consume inbox.Handler
}

// Build wires the Postgres-backed service.
func Build(o Options) (*App, error) {
	cat, err := gen.Catalogue()
	if err != nil {
		return nil, err
	}
	if o.Clock == nil {
		o.Clock = clock.System
	}
	if o.AuthClock == nil {
		o.AuthClock = o.Clock
	}
	if o.IDs == nil {
		o.IDs = ids.NewGenerator(o.Clock.Now, nil)
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	repo := pg.New(o.AppDB)
	svc := service.New(repo, service.Options{Clock: o.Clock, IDs: o.IDs, Log: o.Log, Emitter: o.Emitter, TaxClasses: o.TaxClasses})
	guard := &auth.Guard{Catalogue: cat, Tenant: o.Tenant, Services: o.Services, Clock: o.AuthClock, Log: o.Log, Audit: &pg.AuditSink{DB: o.AppDB, Log: o.Log}}
	h := health.New(0)
	h.Add("postgres", repo.Ping)
	return &App{
		Handler: api.New(api.Config{Service: svc, Guard: guard, Clock: o.Clock, Log: o.Log, Health: h}),
		Service: svc, Repo: repo, Health: h,
		Consume: func(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
			rtx, err := pg.Bind(ctx, tx, env.TenantID)
			if err != nil {
				return err
			}
			return svc.ApplyEvent(ctx, rtx, env)
		},
	}, nil
}
