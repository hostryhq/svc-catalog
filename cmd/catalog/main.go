// Command catalog is the catalog service: menus, items, variants, modifiers, tax classes, stations, tables,
// printers and the menu import (ADR-0014). Wiring only.
//
//	catalog serve     (default) run the HTTP API, the outbox relay and the bus consumers
//	catalog migrate   apply the SQL migrations as the migrator role (separate step; never at serve time)
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/bus"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/inbox"
	"github.com/hostryhq/go-common/logx"
	"github.com/hostryhq/go-common/outbox"

	"github.com/hostryhq/svc-catalog/internal/app"
	"github.com/hostryhq/svc-catalog/internal/config"
	"github.com/hostryhq/svc-catalog/repository/migrations"
	"github.com/hostryhq/svc-catalog/repository/pg"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load(os.LookupEnv)
	if cmd == "migrate" {
		err = nil
		if cfg.MigrateDSN == "" {
			err = errors.New("CATALOG_DB_MIGRATE_DSN is required for migrate")
		}
	}
	log := logx.New(os.Stderr, slog.LevelInfo, "catalog")
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "migrate":
		err = migrate(ctx, cfg)
	case "serve":
		err = serve(ctx, cfg, log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

func migrate(ctx context.Context, cfg config.Config) error {
	db, err := pg.Open(cfg.MigrateDSN, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return migrations.Apply(ctx, db)
}

// keyLoader loads a JWKS document from a URL or a file path.
type keyLoader struct{ src string }

func (k keyLoader) Load(ctx context.Context) (auth.KeyDoc, error) {
	var b []byte
	if len(k.src) > 8 && (k.src[:7] == "http://" || k.src[:8] == "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.src, nil)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return auth.KeyDoc{}, err
		}
	} else {
		var err error
		if b, err = os.ReadFile(k.src); err != nil {
			return auth.KeyDoc{}, err
		}
	}
	keys, err := auth.ParseJWKS(b)
	return auth.KeyDoc{Keys: keys}, err
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	appDB, err := pg.Open(cfg.AppDSN, 20)
	if err != nil {
		return err
	}
	defer func() { _ = appDB.Close() }()
	// TC-01: refuse to run as a superuser, a BYPASSRLS role or a table owner.
	if err := pg.VerifyRuntimeRole(ctx, appDB); err != nil {
		return fmt.Errorf("app pool: %w", err)
	}
	keys := auth.NewCachedKeySet(keyLoader{cfg.TenantJWKS}, log)
	if err := keys.Refresh(ctx); err != nil {
		return fmt.Errorf("load signing keys: %w", err)
	}
	go keys.Run(ctx, cfg.KeyRefresh)

	// The gateway emit client (ADR-0014 A2.2): interactive edits call it, and the emit_queue worker below drains
	// queued emissions with it. Without CATALOG_SYNC_URL and the signing key, edits answer 503 emit.unavailable
	// and queued emissions wait.
	ec, err := emitClient(cfg)
	if err != nil {
		return err
	}
	var emitter emit.Emitter
	if ec != nil {
		emitter = ec
	}
	var svcVerifier *auth.ServiceVerifier
	if cfg.ServiceKeys != "" {
		doc, err := keyLoader{cfg.ServiceKeys}.Load(ctx)
		if err != nil {
			return fmt.Errorf("load service keys: %w", err)
		}
		svcVerifier = &auth.ServiceVerifier{Registry: auth.NewStaticKeySet(doc), Self: "catalog"}
	}
	a, err := app.Build(app.Options{AppDB: appDB, Log: log, TaxClasses: cfg.TaxClasses, Services: svcVerifier, Emitter: emitter,
		Tenant: &auth.Verifier{Keys: keys, Issuer: cfg.TenantIssuer, Audience: cfg.TenantAudience, Realm: auth.RealmTenant}})
	if err != nil {
		return err
	}
	if cfg.NatsURL != "" && cfg.RelayDSN != "" {
		go relay(ctx, cfg, log)
		if ec != nil {
			go emitWorker(ctx, cfg, ec, log)
		}
		go consume(ctx, cfg, appDB, a, log)
	} else {
		log.Warn("bus disabled: NATS_URL or CATALOG_DB_RELAY_DSN not set; events stay in the outbox and nothing is consumed")
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("catalog listening", "addr", cfg.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

func relay(ctx context.Context, cfg config.Config, log *slog.Logger) {
	db, err := pg.Open(cfg.RelayDSN, 2)
	if err != nil {
		log.Error("relay: open database", "error", err)
		return
	}
	defer func() { _ = db.Close() }()
	if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
		log.Error("relay: refusing to start", "error", err)
		return
	}
	nc, err := bus.Connect(cfg.NatsURL, "catalog", cfg.NatsSeedFile)
	if err != nil {
		log.Error("relay: connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	pub, err := bus.NewPublisher(nc)
	if err != nil {
		log.Error("relay: publisher", "error", err)
		return
	}
	r := &outbox.Relay{DB: db, Sink: pub, LockKey: 7_464_012_003, Log: log}
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("relay stopped", "error", err)
	}
}

// emitClient builds the gateway emit client, or nil when it is not configured. The service token is signed per
// call with this service's key (a deploy secret, read from a file by reference, never logged).
func emitClient(cfg config.Config) (*emit.Client, error) {
	if cfg.SyncURL == "" {
		return nil, nil
	}
	if cfg.ServiceSigningKey == "" {
		return nil, errors.New("CATALOG_SERVICE_SIGNING_KEY is required with CATALOG_SYNC_URL")
	}
	raw, err := os.ReadFile(cfg.ServiceSigningKey)
	if err != nil {
		return nil, fmt.Errorf("read service signing key: %w", err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("service signing key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(blk.Bytes)
		ek, ok := k.(*ecdsa.PrivateKey)
		if perr != nil || !ok {
			return nil, errors.New("service signing key is not an EC private key")
		}
		key = ek
	}
	signer := &auth.ServiceSigner{Name: "catalog", Key: key}
	return &emit.Client{BaseURL: cfg.SyncURL, Token: func(context.Context) (string, error) { return signer.Sign("sync", "") },
		HTTP: &http.Client{Timeout: 10 * time.Second}}, nil
}

// emitWorker drains emit_queue (queued emissions: menu import commits) as the relay role, which sees every
// tenant's rows of that table only. Rows are sent in order per tenant with back-off while the gateway is down.
func emitWorker(ctx context.Context, cfg config.Config, c *emit.Client, log *slog.Logger) {
	db, err := pg.Open(cfg.RelayDSN, 2)
	if err != nil {
		log.Error("emit worker: open database", "error", err)
		return
	}
	defer func() { _ = db.Close() }()
	if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
		log.Error("emit worker: refusing to start", "error", err)
		return
	}
	q := &emit.Queue{DB: db, Emitter: c, Log: log}
	if err := q.Run(ctx, 2*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("emit worker stopped", "error", err)
	}
}

// consume runs one inbox subscriber per publisher catalog reads: the gateway's op events and tenancy's outlet
// events (ADR-0011 B5, B10; no order is needed for sync ops, ADR-0014 A2.1 item 5).
func consume(ctx context.Context, cfg config.Config, db *sql.DB, a *app.App, log *slog.Logger) {
	nc, err := bus.Connect(cfg.NatsURL, "catalog", cfg.NatsSeedFile)
	if err != nil {
		log.Error("consume: connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	for _, publisher := range []string{"sync", "tenancy"} {
		c, err := bus.BindConsumer(ctx, nc, publisher, "catalog")
		if err != nil {
			log.Error("consume: bind consumer", "publisher", publisher, "error", err)
			continue
		}
		sub := &inbox.Subscriber{DB: db, Consumer: "catalog:" + publisher, Handle: a.Consume, Log: log}
		go func() {
			if err := sub.Run(ctx, c.Source(5*time.Second)); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("consumer stopped", "publisher", publisher, "error", err)
			}
		}()
	}
	<-ctx.Done()
}
