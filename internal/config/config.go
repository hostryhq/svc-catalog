// Package config reads the service configuration from the environment (12-factor) and fails fast with every
// problem at once. Secrets are database credentials and key material by reference only: a DSN or a file path,
// never a literal in code or logs.
package config

import (
	"strings"
	"time"

	"github.com/hostryhq/go-common/config"
)

// Config is the validated configuration.
type Config struct {
	Addr string
	// AppDSN connects as catalog_app, RelayDSN as catalog_relay (outbox only).
	AppDSN, RelayDSN string
	// MigrateDSN connects as the migrator; used only by `catalog migrate`.
	MigrateDSN string
	// TenantJWKS is the JWKS document (URL or file path) of the tenant token issuer.
	// ServiceKeys is the service key registry (a JWKS file whose kid is the service name); optional. Without
	// it support sessions (which travel with the admin service token) are refused.
	ServiceKeys                  string
	TenantJWKS                   string
	TenantIssuer, TenantAudience string
	// TaxClasses are the tax classes an owner may choose; the rule set in force is billing's data.
	TaxClasses []string
	// NatsURL and NatsSeedFile connect the outbox relay and the consumers to the bus; empty disables both.
	NatsURL, NatsSeedFile string
	KeyRefresh            time.Duration
	LogLevel              string
}

// Load reads the environment through lookup.
func Load(lookup config.Lookup) (Config, error) {
	l := config.New(lookup)
	c := Config{
		Addr:           l.String("CATALOG_ADDR", ":8080"),
		AppDSN:         l.Required("CATALOG_DB_APP_DSN"),
		RelayDSN:       l.String("CATALOG_DB_RELAY_DSN", ""),
		MigrateDSN:     l.String("CATALOG_DB_MIGRATE_DSN", ""),
		TenantJWKS:     l.Required("CATALOG_TENANT_JWKS"),
		TenantIssuer:   l.String("CATALOG_TENANT_ISSUER", "hostry-identity"),
		TenantAudience: l.String("CATALOG_TENANT_AUDIENCE", "hostry-tenant"),
		ServiceKeys:    l.String("CATALOG_SERVICE_KEYS", ""),
		NatsURL:        l.String("NATS_URL", ""),
		NatsSeedFile:   l.String("NATS_SEED_FILE", ""),
		KeyRefresh:     l.Duration("CATALOG_KEY_REFRESH", time.Minute),
		LogLevel:       l.String("LOG_LEVEL", "info"),
	}
	for _, t := range strings.Split(l.String("CATALOG_TAX_CLASSES", "fnb_standard,fnb_non_gst,exempt"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.TaxClasses = append(c.TaxClasses, t)
		}
	}
	return c, l.Err()
}
