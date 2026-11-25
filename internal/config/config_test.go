package config

import (
	"strings"
	"testing"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadFailsFastWithEveryProblem(t *testing.T) {
	_, err := Load(lookup(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "CATALOG_DB_APP_DSN") || !strings.Contains(err.Error(), "CATALOG_TENANT_JWKS") {
		t.Fatalf("all missing settings must be reported at once: %v", err)
	}
}

func TestLoadDefaultsAndTaxClasses(t *testing.T) {
	c, err := Load(lookup(map[string]string{"CATALOG_DB_APP_DSN": "dsn", "CATALOG_TENANT_JWKS": "/keys.json", "CATALOG_TAX_CLASSES": "fnb_standard, exempt ,"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || len(c.TaxClasses) != 2 || c.TaxClasses[1] != "exempt" || c.NatsURL != "" {
		t.Fatalf("defaults wrong: %+v", c)
	}
}
