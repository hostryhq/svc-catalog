package double_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/contractenv"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

// examplesDiffer lists the operations whose spec example cannot equal what the double computes, with the reason
// (spec problems reported to api-specs). They still run the schema, status and replay checks.
var examplesDiffer = map[string]string{
	"createMenuImport":     "example response says one category and one item are created, but the base world already holds category Starters and item code 101",
	"commitMenuImport":     "example committed_at 13:09:40 differs from the fixed double clock",
	"listItems":            "example page carries a next_cursor although the page is the last one",
	"listItemAvailability": "the example query asks available=false but the example row is available",
	"setCategoryTaxClass":  "example op_id cloud-1/brand:0190a1b2/412 is a truncated id and a fixed sequence",
	"setItemTaxClass":      "example op_id cloud-1/brand:0190a1b2/412 is a truncated id and a fixed sequence",
}

func client(env *double.Env) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &tokenTransport{tok: env.TenantToken(seed.Example.Org())}}
}

type tokenTransport struct{ tok string }

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func run(t *testing.T, expect bool, skip map[string]string) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	contracttest.Run(t, contracttest.Options{SpecPath: contractenv.SpecPath(t), BaseURL: srv.URL, Client: client(env), ExpectExamples: expect, Skip: skip, SeedWorld: env.SeedWorld})
}

// TestContractExamples runs every operation against the double, which must serve the spec's own examples.
func TestContractExamples(t *testing.T) {
	skip := map[string]string{}
	for op := range examplesDiffer {
		skip[op] = "run in TestContractSchemaOnly"
	}
	run(t, true, skip)
}

// TestContractSchemaOnly runs the operations whose example cannot be reproduced exactly: status, content type,
// schema and idempotent replay are still checked.
func TestContractSchemaOnly(t *testing.T) {
	skip := map[string]string{}
	for _, op := range []string{"listCategories", "getItem", "getMenu", "listModifierGroups", "listReasonCodes", "listStations", "listFloors",
		"listDiningTables", "listPrinters", "listCatalogExceptions", "listMenuImports", "getMenuImport", "cancelMenuImport", "unknown-route"} {
		skip[op] = "run in TestContractExamples"
	}
	run(t, false, skip)
}
