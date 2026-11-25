package httpsuite

import (
	"testing"

	"github.com/hostryhq/svc-catalog/internal/seed"
)

// SupportImport covers the support act-mode import (ADR-0005 A2.5 item 5): a support token in act mode that
// travels with the admin service token is judged on catalog.import, may add items, and is refused 422
// owner_commit_required for any change to an existing item or price, storing nothing; without the admin service
// token it is refused.
func SupportImport(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Example
	g.Load(t, w, seed.Base)
	org := w.Org()
	const hdr = "category,item name,item code,variant name,price,veg mark,station\n"
	add := obj{"filename": "menu.csv", "csv_text": hdr + "Mains,Soup,401,,99.00,veg,Kitchen\n"}
	change := obj{"filename": "menu.csv", "csv_text": hdr + "Starters,Paneer Tikka,101,Full,250.00,veg,Kitchen\n"}
	act := g.SupportActHeaders(org, true, "catalog.import")
	jobs := func() int { return len(ids_(t, g.Call(t, "GET", "/v1/menu-imports", g.Owner(org), nil), "id")) }

	r := g.Call(t, "POST", "/v1/menu-imports", act, add)
	want(t, r, 201, "")
	if j := r.JSON(t); j["source"] != "support_session" || j["items_created"] != float64(1) || j["items_updated"] != float64(0) {
		t.Fatalf("a support import that only adds is previewed as a support session: %s", r.Body)
	}
	before := jobs()
	want(t, g.Call(t, "POST", "/v1/menu-imports", act, change), 422, "owner_commit_required")
	if n := jobs(); n != before {
		t.Fatalf("a refused support import stores no job: %d, was %d", n, before)
	}
	want(t, g.Call(t, "POST", "/v1/menu-imports", g.SupportActHeaders(org, false, "catalog.import"), add), 401, "")
	if n := jobs(); n != before {
		t.Fatalf("a support token without the admin service token stores nothing: %d, was %d", n, before)
	}
}
