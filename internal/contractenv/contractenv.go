// Package contractenv prepares the shared contract suite (api-specs/contracttest) for this service. It is used by
// the contract tests of the double and of the real service, so both run the same checks.
//
// The suite sends each operation once with the spec's example inputs, after SeedWorld has put the target into
// the operation's example world (internal/seed). This package supplies the spec files.
package contractenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shared X-Outlet-Id parameter (common/components.yaml) carries the example 4a5c, an id no entity uses; the
// catalog examples are all about outlet 4a61. SpecPath rewrites it in a temporary copy (spec problem reported
// to api-specs).
var fixes = map[string]string{"0c1d2e3f4a5c": "0c1d2e3f4a61"}

// SpecPath returns the path of a patched copy of the catalog OpenAPI document (with common/ beside it). The
// api-specs checkout comes from API_SPECS_DIR, or from the module this repo resolves (the pinned version).
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
	dst := t.TempDir()
	for _, d := range []string{"openapi/catalog/v1", "common"} {
		if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, d))
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, d, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for from, to := range fixes {
				s = strings.ReplaceAll(s, from, to)
			}
			if err := os.WriteFile(filepath.Join(dst, d, e.Name()), []byte(s), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return filepath.Join(dst, "openapi/catalog/v1/openapi.yaml")
}
