package double

import (
	"net/http"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/ids"
)

// SupportActHeaders returns a support-session token in act mode for tenant with the given permissions. The admin
// service token a support token must travel with (ADR-0013 V2 item 5) is added only when withAdmin is true.
func (e *Tokens) SupportActHeaders(tenant ids.ID, withAdmin bool, perms ...string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeSupport, Mode: auth.ModeAct, Tenant: tenant, Grants: authtest.Grants(nil, perms...)}))
	if withAdmin {
		tok, err := e.Registry.Signer("admin").Sign("catalog", tenant.String())
		if err != nil {
			panic(err)
		}
		h.Set(auth.ServiceTokenHeader, tok)
	}
	return h
}
