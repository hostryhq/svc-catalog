package double

import (
	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
)

// Tokens are the test issuer a service under test trusts, with helpers that mint the credentials each kind of
// request needs. For tests only: tokens are signed with throw-away keys.
type Tokens struct {
	TenantIssuer *authtest.Issuer
}

// NewTokens creates the issuer.
func NewTokens() *Tokens { return &Tokens{TenantIssuer: authtest.NewTenantIssuer(clock.System)} }

// Env is a double together with the tokens its verifier trusts.
type Env struct {
	*Double
	*Tokens
}

// NewEnv builds a double that trusts a fresh test issuer.
func NewEnv() (*Env, error) {
	tk := NewTokens()
	d, err := New(Config{Tenant: tk.TenantIssuer.Verifier()})
	if err != nil {
		return nil, err
	}
	return &Env{Double: d, Tokens: tk}, nil
}

// TenantToken mints a strong (OTP, fresh) user token for tenant with the given grants. Without grants it is an
// owner: every permission at every outlet.
func (e *Tokens) TenantToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Tenant: tenant, Subject: "member-1", Grants: grants})
}

// PinToken mints a shared-device PIN session token, which can never exercise strong-only permissions.
func (e *Tokens) PinToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypePin, Tenant: tenant, Grants: grants})
}

// SupportToken mints a support-session token in view mode for tenant.
func (e *Tokens) SupportToken(tenant ids.ID, perms ...string) string {
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeSupport, Tenant: tenant, Grants: authtest.Grants(nil, perms...)})
}
