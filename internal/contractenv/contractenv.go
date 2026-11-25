// Package contractenv locates the OpenAPI document the shared contract suite (api-specs/contracttest) runs
// against. It is used by the contract tests of the double and of the real service, so both run the same checks,
// each operation after SeedWorld has put the target into the operation's example world (internal/seed).
package contractenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SpecPath returns the path of the catalog OpenAPI document. The api-specs checkout comes from API_SPECS_DIR, or
// from the module this repo resolves (the pinned version, or the workspace checkout during development).
func SpecPath(t testing.TB) string {
	t.Helper()
	root := os.Getenv("API_SPECS_DIR")
	if root == "" {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/hostryhq/api-specs").Output()
		if err != nil {
			t.Fatalf("locate api-specs: %v", err)
		}
		root = strings.TrimSpace(string(out))
	}
	return filepath.Join(root, "openapi/catalog/v1/openapi.yaml")
}
