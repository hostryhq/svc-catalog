// Package api is the controller layer: it authenticates through go-auth, validates the transport
// (strict JSON, Idempotency-Key, path ids), calls the service and maps results to the generated DTOs.
// It holds no business rule. Routes and their permissions are generated from the OpenAPI document
// (internal/gen.Policies), so a route cannot be registered without a declared permission.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/health"
	"github.com/hostryhq/go-common/httpx"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/page"
	"github.com/hostryhq/svc-catalog/service"
)

// Config wires the handler.
type Config struct {
	Service *service.Service
	Guard   *auth.Guard
	Clock   clock.Clock
	Log     *slog.Logger
	// Health, when set, mounts /healthz and /readyz.
	Health *health.Handler
}

// Server implements gen.ServerInterface.
type Server struct {
	svc *service.Service
	clk clock.Clock
	log *slog.Logger
}

var _ gen.ServerInterface = (*Server)(nil)

// New builds the HTTP handler: request id, panic recovery, generated routes behind the go-auth guard.
func New(cfg Config) http.Handler {
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{svc: cfg.Service, clk: cfg.Clock, log: cfg.Log}
	mux := http.NewServeMux()
	rt := &router{mux: mux, cfg: cfg}
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: rt, ErrorHandlerFunc: badParam})
	if cfg.Health != nil {
		cfg.Health.Register(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteProblem(w, r, apperr.New(apperr.NotFound, "not_found", ""))
	})
	return httpx.Chain(mux, httpx.RequestID, httpx.Recover)
}

// router registers each generated route behind the go-auth guard with the policy declared in the spec.
type router struct {
	mux *http.ServeMux
	cfg Config
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	p, ok := gen.Policies[pattern]
	if !ok {
		panic("api: no policy generated for " + pattern)
	}
	pol := auth.Policy{OperationID: p.OperationID, Permission: p.Permission, StepUp: p.StepUp}
	var handler http.Handler = rt.cfg.Guard.Wrap(pol, http.HandlerFunc(h))
	if p.StepUp {
		handler = strongHint(handler)
	}
	rt.mux.Handle(pattern, handler)
}

// badParam answers parameter binding errors from the generated wrappers.
func badParam(w http.ResponseWriter, r *http.Request, err error) {
	var req *gen.RequiredHeaderError
	if errors.As(err, &req) && strings.EqualFold(req.ParamName, idempotency.Header) {
		apperr.WriteProblem(w, r, idempotency.ErrKeyInvalid)
		return
	}
	var f apperr.FieldError
	var inv *gen.InvalidParamFormatError
	if errors.As(err, &inv) {
		f = apperr.FieldError{Field: inv.ParamName, Code: "invalid"}
	}
	var missing *gen.RequiredParamError
	if errors.As(err, &missing) {
		f = apperr.FieldError{Field: missing.ParamName, Code: "required"}
	}
	apperr.WriteProblem(w, r, apperr.New(apperr.Validation, "validation", "invalid request parameter").WithFields(f))
}

// ---- transport helpers ----

type ctxT = context.Context

const maxBody = 1 << 20

// begin prepares a mutating request: it decodes the body strictly into body (an unknown field, such as a
// tenant id, is a 422), validates the Idempotency-Key and puts the idempotency identity on the context.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, op string, body any) (context.Context, bool) {
	ctx := r.Context()
	var canonical []byte
	if body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			s.fail(w, r, apperr.New(apperr.Validation, "validation", "request body unreadable"))
			return nil, false
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil || dec.More() {
			s.fail(w, r, decodeError(err))
			return nil, false
		}
		canonical, _ = json.Marshal(body)
	}
	key, ok, err := idempotency.KeyFromRequest(r)
	if err != nil || !ok {
		s.fail(w, r, idempotency.ErrKeyInvalid)
		return nil, false
	}
	return service.WithIdem(ctx, service.Idem{Op: op, Key: key, Fingerprint: idempotency.Fingerprint(r.Method, r.URL.Path, canonical)}), true
}

func decodeError(err error) error {
	field := "body"
	code := "malformed"
	if err != nil {
		if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
			field, code = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`), "unknown_field"
		}
	}
	return apperr.New(apperr.Validation, "validation", "invalid request body").WithFields(apperr.FieldError{Field: field, Code: code})
}

// pathID parses a path id. A malformed id is the same 404 as a missing one.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request, raw string) (ids.ID, bool) {
	id, err := ids.Parse(raw)
	if err != nil {
		s.fail(w, r, apperr.New(apperr.NotFound, "not_found", ""))
		return "", false
	}
	return id, true
}

// fail writes err as a problem. The gateway being down is 503 emit.unavailable with Retry-After (the one code of
// every service, ADR-0014 A2.2 item 7); a gateway refusal is a defect here and is 500 emit.rejected.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var rej *emit.RejectedError
	switch {
	case errors.As(err, &rej):
		s.log.ErrorContext(r.Context(), "catalog: the gateway refused an emit", "status", rej.Status, "code", rej.Code)
		err = rej.AppError()
	case emit.IsTransient(err):
		after := max(emit.RetryAfter(err), time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(int((after+time.Second-1)/time.Second)))
	}
	apperr.WriteProblem(w, r, err)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, merr := json.Marshal(v)
	if merr != nil {
		b = []byte(`{}`)
	}
	_, _ = w.Write(append(millis(b), '\n'))
}

var tsPat = regexp.MustCompile(`"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?Z"`)

// millis writes every RFC 3339 timestamp with exactly three fractional digits, as the spec's examples do.
func millis(b []byte) []byte {
	return tsPat.ReplaceAllFunc(b, func(m []byte) []byte {
		g := tsPat.FindSubmatch(m)
		frac := string(g[2])
		if frac == "" {
			frac = ".000"
		}
		frac = (frac + "000")[:4]
		return []byte(`"` + string(g[1]) + frac + `Z"`)
	})
}

// pageParams validates cursor and limit. A cursor carries the id of the last row seen (any UUID: some rows
// have deterministic UUIDv5 ids).
func pageParams(cursor *string, limit *int, def, max int) (after string, n int, err error) {
	after, err = page.Decode(cursor)
	if err != nil {
		return "", 0, err
	}
	if after != "" && (len(after) > 64 || strings.ContainsAny(after, " \t\n'\"")) {
		return "", 0, apperr.New(apperr.Validation, "validation", "invalid cursor").WithFields(apperr.FieldError{Field: "cursor", Code: "invalid"})
	}
	n, err = page.Limit(limit, def, max)
	return after, n, err
}
