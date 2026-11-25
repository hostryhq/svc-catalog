package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hostryhq/go-common/apperr"
)

// CodeStrongSessionRequired is the stable code the contract declares for a strong-only operation tried from a
// pin or device session (RM-M5). go-auth answers every ceiling with auth.permission_denied; this layer
// refines that one answer when the (already refused) request carried a pin or device token. It never
// grants anything: the guard has refused before this code runs.
const CodeStrongSessionRequired = "strong_session_required"

// strongHint wraps a guarded strong-only route.
func strongHint(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := unverifiedTokenType(r); t != "pin" && t != "device" {
			next.ServeHTTP(w, r)
			return
		}
		c := &capture{ResponseWriter: w}
		next.ServeHTTP(c, r)
		if !c.held {
			return
		}
		if bytes.Contains(c.buf.Bytes(), []byte("auth.permission_denied")) {
			apperr.WriteProblem(w, r, apperr.New(apperr.Forbidden, CodeStrongSessionRequired, "this action needs a strong session"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(c.buf.Bytes())
	})
}

// capture holds back a 403 body so it can be refined; every other answer passes through.
type capture struct {
	http.ResponseWriter
	held   bool
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(code int) {
	c.status = code
	if code == http.StatusForbidden {
		c.held = true
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	if c.held {
		return c.buf.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// unverifiedTokenType reads the typ claim of the bearer token without verifying it. It is used only to pick
// the wording of a refusal that has already happened.
func unverifiedTokenType(r *http.Request) string {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Typ string `json:"typ"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Typ
}
