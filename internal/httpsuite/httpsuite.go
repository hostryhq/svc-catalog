// Package httpsuite holds HTTP-level suites that run against the real service on Postgres and against the
// in-memory test double alike: tenant isolation (ADR-0003 T2), outlet scoping (T14), authorisation (strong
// session, support audit), the tax-class edits and the menu import. A suite that passes for both keeps the
// double honest.
package httpsuite

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-catalog/double"
	"github.com/hostryhq/svc-catalog/internal/itest"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

type obj = map[string]any

// Target is a freshly started service with no data.
type Target struct {
	Base string
	*double.Tokens
	// Load stores an organisation's named example world.
	Load func(t testing.TB, w seed.World, world string)
	// Emitted returns the ops the gateway stored, in order.
	Emitted func() []double.Emitted
	// SetGatewayDown makes the sync gateway unreachable or reachable again; Settle then runs the emit worker and
	// delivers what the gateway stored, as the process's worker and the bus do.
	SetGatewayDown func(down bool)
	Settle         func(t testing.TB)
	// EventTypes returns the types of the events in the tenant's outbox, in order.
	EventTypes func(t testing.TB, tenant ids.ID) []string
	// AuditActions returns the audit_log actions of the tenant, in order.
	AuditActions func(t testing.TB, tenant ids.ID) []string
	// VendorLog returns the action keys of the tenant's vendor_access_log, in order.
	VendorLog func(t testing.TB, tenant ids.ID) []string
}

// Factory starts a new Target.
type Factory func(t *testing.T) *Target

// Call sends a request with the given headers.
func (g *Target) Call(t testing.TB, method, path string, h http.Header, body any) itest.Response {
	t.Helper()
	return itest.Do(t, g.Base, method, path, h, body)
}

// Owner returns headers of an owner of tenant (strong session, every permission at every outlet).
func (g *Target) Owner(tenant ids.ID) http.Header { return itest.Bearer(g.TenantToken(tenant)) }

func want(t testing.TB, r itest.Response, status int, code string) {
	t.Helper()
	if r.Status != status || (code != "" && r.Code() != code) {
		t.Fatalf("status %d code %q, want %d %q: %s", r.Status, r.Code(), status, code, r.Body)
	}
}

func ids_(t testing.TB, r itest.Response, key string) []string {
	t.Helper()
	want(t, r, 200, "")
	var p struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range p.Items {
		s, _ := i[key].(string)
		out = append(out, s)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func outletHeader(base http.Header, outlet ids.ID) http.Header {
	return itest.Merge(base, http.Header{"X-Outlet-Id": []string{outlet.String()}})
}

// Isolation proves tenant A's menu, set-up and jobs are invisible and untouchable for tenant B.
func Isolation(t *testing.T, mk Factory) {
	g := mk(t)
	a, b := seed.Example, seed.Other
	g.Load(t, a, seed.BaseWithImport)
	g.Load(t, b, seed.Base)
	ownerB := g.Owner(b.Org())
	ownerA := g.Owner(a.Org())

	t.Run("tenant B gets 404 for tenant A's resources", func(t *testing.T) {
		reason := obj{"tax_class": "exempt", "reason": "Not allowed to touch it."}
		for _, c := range []struct {
			name, method, path string
			body               any
		}{
			{"get item", "GET", "/v1/items/" + a.Item().String(), nil},
			{"item tax class", "PUT", "/v1/items/" + a.Item().String() + "/tax-class", reason},
			{"category tax class", "PUT", "/v1/categories/" + a.Category().String() + "/tax-class", reason},
			{"get import", "GET", "/v1/menu-imports/" + a.Import().String(), nil},
			{"cancel import", "POST", "/v1/menu-imports/" + a.Import().String() + "/cancellation", nil},
			{"commit import", "POST", "/v1/menu-imports/" + a.Import().String() + "/commit", obj{"row_count": 1}},
		} {
			t.Run(c.name, func(t *testing.T) { want(t, g.Call(t, c.method, c.path, ownerB, c.body), 404, "not_found") })
		}
		// Tenant A's outlet named in X-Outlet-Id is the same 404 as a missing one, for every outlet-scoped read.
		for _, p := range []string{"/v1/stations", "/v1/floors", "/v1/dining-tables", "/v1/printers", "/v1/item-availability", "/v1/exceptions", "/v1/menu", "/v1/items"} {
			want(t, g.Call(t, "GET", p, outletHeader(ownerB, a.Outlet()), nil), 404, "not_found")
		}
	})

	t.Run("lists hold only the caller's own rows", func(t *testing.T) {
		for _, c := range []struct{ path, key string }{{"/v1/items", "id"}, {"/v1/categories", "id"}, {"/v1/modifier-groups", "id"}, {"/v1/reason-codes", "id"},
			{"/v1/stations", "id"}, {"/v1/floors", "id"}, {"/v1/dining-tables", "id"}, {"/v1/printers", "id"}, {"/v1/menu-imports", "id"}} {
			own, other := ids_(t, g.Call(t, "GET", c.path, ownerB, nil), c.key), ids_(t, g.Call(t, "GET", c.path, ownerA, nil), c.key)
			for _, id := range own {
				if contains(other, id) {
					t.Errorf("%s: row %s is listed for both tenants", c.path, id)
				}
				if !strings.HasPrefix(id, b.Prefix) {
					t.Errorf("%s: tenant B sees a row of another tenant: %s", c.path, id)
				}
			}
		}
		if n := len(ids_(t, g.Call(t, "GET", "/v1/menu-imports", ownerB, nil), "id")); n != 0 {
			t.Errorf("tenant B has no import job, saw %d", n)
		}
	})

	t.Run("a refused attempt changes nothing", func(t *testing.T) {
		if n := len(g.Emitted()); n != 0 {
			t.Fatalf("no op may have been emitted, have %d", n)
		}
		r := g.Call(t, "GET", "/v1/items/"+a.Item().String(), ownerA, nil)
		want(t, r, 200, "")
		if r.JSON(t)["tax_class"] != "fnb_standard" {
			t.Fatal("tenant A's item changed")
		}
	})
}

// OutletScope proves a member scoped to one outlet sees nothing of another (ADR-0003 A1.4, T14), through the
// HTTP surface.
func OutletScope(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Other
	g.Load(t, w, seed.Base)
	scoped := func(outlet ids.ID, perms ...string) http.Header {
		return itest.Bearer(g.TenantToken(w.Org(), authtest.Grants([]string{outlet.String()}, perms...)...))
	}
	readX := scoped(w.Outlet(), "menu.read", "settings.read", "exceptions.view")

	t.Run("a scoped member lists only their outlet", func(t *testing.T) {
		for _, p := range []string{"/v1/stations", "/v1/floors", "/v1/dining-tables", "/v1/printers", "/v1/item-availability"} {
			r := g.Call(t, "GET", p, readX, nil)
			want(t, r, 200, "")
			var page struct {
				Items []struct {
					OutletID string `json:"outlet_id"`
				} `json:"items"`
			}
			_ = json.Unmarshal(r.Body, &page)
			if len(page.Items) != 1 || page.Items[0].OutletID != w.Outlet().String() {
				t.Errorf("%s: want exactly outlet X's row, got %s", p, r.Body)
			}
		}
	})
	t.Run("another outlet is 404, named or not", func(t *testing.T) {
		for _, p := range []string{"/v1/stations", "/v1/menu", "/v1/item-availability", "/v1/exceptions"} {
			want(t, g.Call(t, "GET", p, outletHeader(readX, w.OutletY()), nil), 404, "not_found")
		}
	})
	t.Run("the menu of a single-outlet member needs no header", func(t *testing.T) {
		r := g.Call(t, "GET", "/v1/menu", readX, nil)
		want(t, r, 200, "")
		if r.JSON(t)["outlet_id"] != w.Outlet().String() {
			t.Fatalf("menu is for outlet X: %s", r.Body)
		}
	})
	t.Run("an organisation-wide member sees both and may pick one", func(t *testing.T) {
		owner := g.Owner(w.Org())
		both := ids_(t, g.Call(t, "GET", "/v1/stations", owner, nil), "outlet_id")
		if !contains(both, w.Outlet().String()) || !contains(both, w.OutletY().String()) {
			t.Fatalf("both outlets expected: %v", both)
		}
		only := ids_(t, g.Call(t, "GET", "/v1/stations", outletHeader(owner, w.OutletY()), nil), "outlet_id")
		if len(only) != 1 || only[0] != w.OutletY().String() {
			t.Fatalf("the header narrows to outlet Y: %v", only)
		}
		want(t, g.Call(t, "GET", "/v1/menu", owner, nil), 422, "validation") // two outlets: the header is needed
	})
	t.Run("without the permission nothing is returned", func(t *testing.T) {
		want(t, g.Call(t, "GET", "/v1/stations", scoped(w.Outlet(), "menu.read"), nil), 403, "auth.permission_denied")
		want(t, g.Call(t, "GET", "/v1/items", scoped(w.Outlet(), "settings.read"), nil), 403, "auth.permission_denied")
	})
	t.Run("a cursor cannot probe another outlet's rows", func(t *testing.T) {
		yRows := ids_(t, g.Call(t, "GET", "/v1/stations", outletHeader(g.Owner(w.Org()), w.OutletY()), nil), "id")
		cur := encodeCursor(yRows[0])
		r := g.Call(t, "GET", "/v1/stations?cursor="+cur, readX, nil)
		want(t, r, 200, "")
		for _, id := range ids_(t, r, "id") {
			if id == yRows[0] {
				t.Fatal("outlet Y's row leaked through the cursor")
			}
		}
	})
}

func encodeCursor(id string) string {
	return base64RawURL(`{"after":"` + id + `"}`)
}

// Authorization covers the strong-session rule, idempotency keys, strict bodies and the support audit.
func Authorization(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Example
	g.Load(t, w, seed.BaseWithImport)
	org := w.Org()
	pin := itest.Bearer(g.PinToken(org))
	owner := g.Owner(org)
	tax := obj{"tax_class": "fnb_non_gst", "reason": "Alcohol items are outside GST."}
	csv := obj{"filename": "menu.csv", "csv_text": "category,item name,price\nStarters,Soup,99.00\n"}

	t.Run("strong-only actions are refused in a pin session", func(t *testing.T) {
		want(t, g.Call(t, "PUT", "/v1/items/"+w.Item().String()+"/tax-class", pin, tax), 403, "strong_session_required")
		want(t, g.Call(t, "PUT", "/v1/categories/"+w.Category().String()+"/tax-class", pin, tax), 403, "strong_session_required")
		want(t, g.Call(t, "POST", "/v1/menu-imports", pin, csv), 403, "strong_session_required")
		want(t, g.Call(t, "GET", "/v1/menu-imports", pin, nil), 403, "strong_session_required")
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+w.Import().String()+"/commit", pin, obj{"row_count": 1}), 403, "strong_session_required")
		if n := len(g.Emitted()); n != 0 {
			t.Fatalf("a refused request must emit nothing, emitted %d", n)
		}
	})
	t.Run("a pin session may read the menu", func(t *testing.T) {
		want(t, g.Call(t, "GET", "/v1/items", pin, nil), 200, "")
	})
	t.Run("a stale one-time password is refused", func(t *testing.T) {
		stale := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Tenant: org, Grants: authtest.OwnerGrants(), AuthTime: time.Now().Add(-time.Hour)}))
		r := g.Call(t, "PUT", "/v1/items/"+w.Item().String()+"/tax-class", stale, tax)
		want(t, r, 403, "")
	})
	t.Run("no token is 401", func(t *testing.T) {
		want(t, g.Call(t, "GET", "/v1/items", http.Header{}, nil), 401, "")
	})
	t.Run("a mutation needs an Idempotency-Key and a strict body", func(t *testing.T) {
		h := itest.Merge(owner, http.Header{"Idempotency-Key": []string{""}})
		h.Del("Idempotency-Key")
		r := itest.Do(t, g.Base, "PUT", "/v1/items/"+w.Item().String()+"/tax-class", itest.Merge(owner), tax) // key added by Do
		want(t, r, 200, "")
		req, _ := http.NewRequest("PUT", g.Base+"/v1/items/"+w.Item().String()+"/tax-class", strings.NewReader(`{"tax_class":"exempt","reason":"x"}`))
		req.Header.Set("Authorization", owner.Get("Authorization"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("a missing Idempotency-Key is a 422, got %d", resp.StatusCode)
		}
		r = g.Call(t, "PUT", "/v1/items/"+w.Item().String()+"/tax-class", owner, obj{"tax_class": "exempt", "reason": "x", "tenant_id": "someone-else"})
		want(t, r, 422, "validation")
	})
	t.Run("a support session reads the menu and the read is audited; it cannot write", func(t *testing.T) {
		sup := g.SupportHeaders(org, "menu.read")
		want(t, g.Call(t, "GET", "/v1/items", sup, nil), 200, "")
		if log := g.VendorLog(t, org); len(log) != 1 || log[0] != "support.read.menu" {
			t.Fatalf("the support read must write one vendor_access_log row, have %v", log)
		}
		emitted := len(g.Emitted())
		want(t, g.Call(t, "PUT", "/v1/items/"+w.Item().String()+"/tax-class", sup, tax), 403, "")
		want(t, g.Call(t, "POST", "/v1/menu-imports", sup, csv), 403, "")
		// Without the admin service token a support token is refused.
		bare := itest.Bearer(strings.TrimPrefix(sup.Get("Authorization"), "Bearer "))
		want(t, g.Call(t, "GET", "/v1/items", bare, nil), 401, "")
		if n := len(g.Emitted()) - emitted; n != 0 {
			t.Fatalf("nothing may be emitted by a support session, emitted %d", n)
		}
	})
}

func base64RawURL(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// TaxClass covers the two strong-session edits: one config op to the brand stream, the projection picking it
// up, validation, idempotent replay and the audit record.
func TaxClass(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Example
	g.Load(t, w, seed.Base)
	owner := g.Owner(w.Org())
	itemURL := "/v1/items/" + w.Item().String()
	put := func(h http.Header, url string, body any) itest.Response { return g.Call(t, "PUT", url, h, body) }

	r := put(owner, itemURL+"/tax-class", obj{"tax_class": "fnb_non_gst", "reason": "Alcohol items are outside GST."})
	want(t, r, 200, "")
	if j := r.JSON(t); j["id"] != w.Item().String() || j["tax_class"] != "fnb_non_gst" || !strings.HasPrefix(j["op_id"].(string), "cloud-1/brand:"+w.Brand().String()+"/") {
		t.Fatalf("change not described: %s", r.Body)
	}
	ops := g.Emitted()
	if len(ops) != 1 || ops[0].Stream != "brand:"+w.Brand().String() || len(ops[0].Muts) != 1 || ops[0].Muts[0].E != "item" || ops[0].Muts[0].F["tax_class"] != "fnb_non_gst" {
		t.Fatalf("one set op on the item to the brand stream expected: %+v", ops)
	}
	if ops[0].Muts[0].Prev["tax_class"] == "" {
		t.Fatal("the review field must cite the stamp of the value the editor saw")
	}
	if got := g.Call(t, "GET", itemURL, owner, nil).JSON(t)["tax_class"]; got != "fnb_non_gst" {
		t.Fatalf("the projection must reflect the emitted op, item class is %v", got)
	}

	t.Run("category edit leaves existing items alone", func(t *testing.T) {
		want(t, put(owner, "/v1/categories/"+w.Category().String()+"/tax-class", obj{"tax_class": "exempt", "reason": "Menu reclassified."}), 200, "")
		cats := g.Call(t, "GET", "/v1/categories", owner, nil)
		want(t, cats, 200, "")
		if !strings.Contains(string(cats.Body), `"default_tax_class":"exempt"`) {
			t.Fatalf("category class not updated: %s", cats.Body)
		}
		if got := g.Call(t, "GET", itemURL, owner, nil).JSON(t)["tax_class"]; got != "fnb_non_gst" {
			t.Fatalf("the item keeps its own class, has %v", got)
		}
	})
	t.Run("an unknown class and a missing reason are 422", func(t *testing.T) {
		want(t, put(owner, itemURL+"/tax-class", obj{"tax_class": "luxury_0", "reason": "Made up."}), 422, "validation")
		want(t, put(owner, itemURL+"/tax-class", obj{"tax_class": "exempt", "reason": ""}), 422, "validation")
		want(t, put(owner, "/v1/items/"+ids.New().String()+"/tax-class", obj{"tax_class": "exempt", "reason": "No such item."}), 404, "not_found")
	})
	t.Run("a replay emits once and a changed body under the same key is refused", func(t *testing.T) {
		before := len(g.Emitted())
		h := itest.Merge(owner, http.Header{"Idempotency-Key": []string{"tax-replay-1"}})
		body := obj{"tax_class": "fnb_standard", "reason": "Back to standard."}
		first := put(h, itemURL+"/tax-class", body)
		second := put(h, itemURL+"/tax-class", body)
		want(t, first, 200, "")
		if string(first.Body) != string(second.Body) || len(g.Emitted()) != before+1 {
			t.Fatalf("replay must return the first answer and emit nothing: %d ops", len(g.Emitted())-before)
		}
		want(t, put(h, itemURL+"/tax-class", obj{"tax_class": "exempt", "reason": "Different."}), 422, "idempotency.mismatch")
	})
	t.Run("the change is audited", func(t *testing.T) {
		acts := g.AuditActions(t, w.Org())
		if len(acts) < 3 || acts[0] != "menu.tax_class.edit" {
			t.Fatalf("audit entries expected for the edits: %v", acts)
		}
	})
}

// Import covers the CSV menu import: preview with row errors, all rows or none, emission through the gateway,
// the event, re-import as a price edit and the guard rails.
func Import(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Example
	g.Load(t, w, seed.Base)
	owner := g.Owner(w.Org())
	create := func(csv string) itest.Response {
		return g.Call(t, "POST", "/v1/menu-imports", owner, obj{"filename": "menu.csv", "csv_text": csv})
	}
	const hdr = "category,item name,item code,variant name,price,veg mark,station\n"
	hdr_ := strings.ReplaceAll(hdr, "\n", "\n")

	t.Run("a file with an invalid row previews its errors and cannot be committed", func(t *testing.T) {
		r := create(hdr_ + "Mains,Dosa,201,,60.00,veg,Kitchen\nMains,Idli,202,,x,veg,Kitchen\nMains,Vada,203,,30.00,tasty,Kitchen\n")
		want(t, r, 201, "")
		j := r.JSON(t)
		if j["status"] != "previewed" || j["row_count"] != float64(3) || j["error_count"] != float64(2) {
			t.Fatalf("preview wrong: %s", r.Body)
		}
		errs := j["errors"].([]any)
		first := errs[0].(map[string]any)
		if first["row"] != float64(2) || first["field"] != "price" || first["code"] != "price_invalid" {
			t.Fatalf("row error wrong: %v", errs)
		}
		id := j["id"].(string)
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+id+"/commit", owner, obj{"row_count": 3}), 422, "import_has_errors")
		if len(g.Emitted()) != 0 {
			t.Fatal("nothing may be emitted for a preview with errors (all rows or none)")
		}
	})
	t.Run("a valid file is committed as ops, once", func(t *testing.T) {
		r := create(hdr_ + "Mains,Dosa,201,Plain,60.00,veg,Kitchen\nMains,Dosa,201,Ghee,75.50,veg,Kitchen\nMains,Chicken 65,202,,180,non-veg,Kitchen\n")
		want(t, r, 201, "")
		j := r.JSON(t)
		if j["categories_created"] != float64(1) || j["items_created"] != float64(2) || j["items_updated"] != float64(0) {
			t.Fatalf("counts wrong: %s", r.Body)
		}
		id := j["id"].(string)
		url := "/v1/menu-imports/" + id + "/commit"
		want(t, g.Call(t, "POST", url, owner, obj{"row_count": 99}), 422, "validation")
		c := g.Call(t, "POST", url, owner, obj{"row_count": 3})
		want(t, c, 200, "")
		if c.JSON(t)["status"] != "committed" || c.JSON(t)["committed_at"] == nil {
			t.Fatalf("not committed: %s", c.Body)
		}
		ops := g.Emitted()
		if len(ops) != 2 || ops[0].Intent != "menu.import.apply" || ops[0].Muts[0].E != "category" || ops[1].Muts[0].E != "item" || ops[0].Key == ops[1].Key {
			t.Fatalf("one category op then one item batch expected: %+v", ops)
		}
		if ops[0].Actor == "" || ops[0].Stream != "brand:"+w.Brand().String() || ops[0].Seq != 1 || ops[1].Seq != 2 {
			t.Fatalf("ops are sealed by the gateway on the brand stream in order: %+v", ops)
		}
		want(t, g.Call(t, "POST", url, owner, obj{"row_count": 3}), 409, "import_not_previewed")
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+id+"/cancellation", owner, nil), 409, "import_not_previewed")
		// The ops reach catalog's own projection like any other: the new items are readable.
		items := g.Call(t, "GET", "/v1/items?q=dosa", owner, nil)
		want(t, items, 200, "")
		var page struct {
			Items []struct {
				Name     string `json:"name_en"`
				TaxClass string `json:"tax_class"`
				Variants []struct {
					Name  string `json:"name"`
					Def   bool   `json:"is_default"`
					Price struct {
						Paise int64 `json:"amount_paise"`
					} `json:"price"`
				} `json:"variants"`
			} `json:"items"`
		}
		_ = json.Unmarshal(items.Body, &page)
		if len(page.Items) != 1 || len(page.Items[0].Variants) != 2 || page.Items[0].Variants[0].Name != "Plain" || !page.Items[0].Variants[0].Def ||
			page.Items[0].Variants[0].Price.Paise != 6000 || page.Items[0].Variants[1].Price.Paise != 7550 || page.Items[0].TaxClass != "fnb_standard" {
			t.Fatalf("imported item not as in the file: %s", items.Body)
		}
		if ev := g.EventTypes(t, w.Org()); len(ev) != 1 || ev[0] != "catalog.menu_import.committed" {
			t.Fatalf("one import event expected: %v", ev)
		}
		got := g.Call(t, "GET", "/v1/menu-imports/"+id, owner, nil)
		want(t, got, 200, "")
	})
	t.Run("a re-import matches by code and counts as a price edit", func(t *testing.T) {
		before := len(g.Emitted())
		r := create(hdr_ + "Starters,Paneer Tikka,101,Full,250.00,veg,Kitchen\n")
		want(t, r, 201, "")
		j := r.JSON(t)
		if j["items_updated"] != float64(1) || j["items_created"] != float64(0) || j["categories_created"] != float64(0) || j["price_changes"] != float64(1) {
			t.Fatalf("a matched item with a new price is one update and one price change: %s", r.Body)
		}
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+j["id"].(string)+"/commit", owner, obj{"row_count": 1}), 200, "")
		ops := g.Emitted()[before:]
		var price map[string]any
		for _, m := range ops[len(ops)-1].Muts {
			if m.E == "item" {
				t.Fatalf("an unchanged item name must not be re-set: %+v", m)
			}
			if m.E == "price_entry" {
				price = map[string]any{"k": m.K, "prev": m.Prev["price_paise"], "p": m.F["price_paise"]}
			}
		}
		if price == nil || price["k"] != "set" || price["prev"] == "" || price["p"] != int64(25000) {
			t.Fatalf("the price edit must be a set citing the stamp it saw: %v", price)
		}
		if got := g.Call(t, "GET", "/v1/items/"+w.Item().String(), owner, nil).Body; !strings.Contains(string(got), `"amount_paise":25000`) {
			t.Fatalf("new price not projected: %s", got)
		}
	})
	t.Run("cancel, list and file-level problems", func(t *testing.T) {
		r := create(hdr_ + "Drinks,Lime soda,301,,40,none,Bar\n")
		want(t, r, 201, "")
		id := r.JSON(t)["id"].(string)
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+id+"/cancellation", owner, nil), 200, "")
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+id+"/commit", owner, obj{"row_count": 1}), 409, "import_not_previewed")
		jobs := g.Call(t, "GET", "/v1/menu-imports?limit=2", owner, nil)
		want(t, jobs, 200, "")
		if jobs.JSON(t)["next_cursor"] == nil {
			t.Fatalf("more than two jobs exist, a cursor is expected: %s", jobs.Body)
		}
		want(t, create("name,price\nDosa,10\n"), 422, "validation")
		want(t, create(hdr_), 422, "validation")
		var big strings.Builder
		big.WriteString(hdr_)
		for i := 0; i < 2001; i++ {
			big.WriteString("A,Item" + itoa(i) + ",,,1.00,veg,\n")
		}
		want(t, create(big.String()), 422, "validation")
	})
	t.Run("rows are validated one by one", func(t *testing.T) {
		r := create(hdr_ + ",NoCat,,,1,veg,\nA,,,,1,veg,\nA,X,12345678901,,1,veg,\nA,Y,,,1.234,veg,\nA,Z,,,1,veg,ABCDEFGHIJKLMNOPQRSTUVWXYZ\nA,Dup,,Same,1,veg,\nA,Dup,,Same,2,veg,\nA,Dup,,Other,2,egg,\n")
		want(t, r, 201, "")
		var codes []string
		for _, e := range r.JSON(t)["errors"].([]any) {
			m := e.(map[string]any)
			codes = append(codes, itoa(int(m["row"].(float64)))+":"+m["field"].(string)+":"+m["code"].(string))
		}
		wantCodes := []string{"1:category:required", "2:item name:required", "3:item code:code_invalid", "4:price:price_invalid", "5:station:too_long", "7:variant name:variant_duplicate", "8:veg mark:item_conflict"}
		if strings.Join(codes, ",") != strings.Join(wantCodes, ",") {
			t.Fatalf("row errors\n got %v\nwant %v", codes, wantCodes)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

// Outage covers a sync gateway that is down (ADR-0014 A2.2 item 7): an interactive edit answers 503 emit.unavailable
// with Retry-After and changes nothing, and the client's retry with the same key lands once; a queued emission
// (menu import commit) is accepted at once and reaches the gateway, in order, when it is back.
func Outage(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.Example
	g.Load(t, w, seed.Base)
	owner := g.Owner(w.Org())
	itemURL := "/v1/items/" + w.Item().String()
	tax := obj{"tax_class": "fnb_non_gst", "reason": "Alcohol items are outside GST."}
	keyed := itest.Merge(owner, http.Header{"Idempotency-Key": []string{"outage-edit-1"}})

	g.SetGatewayDown(true)
	r := g.Call(t, "PUT", itemURL+"/tax-class", keyed, tax)
	want(t, r, 503, "emit.unavailable")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("emit.unavailable carries Retry-After")
	}
	if got := g.Call(t, "GET", itemURL, owner, nil).JSON(t)["tax_class"]; got != "fnb_standard" || len(g.Emitted()) != 0 || len(g.AuditActions(t, w.Org())) != 0 {
		t.Fatalf("nothing may change while the gateway is down: class %v, ops %d", got, len(g.Emitted()))
	}

	c := g.Call(t, "POST", "/v1/menu-imports", owner, obj{"filename": "menu.csv", "csv_text": "category,item name,item code,variant name,price,veg mark,station\nMains,Soup,401,,99.00,veg,Kitchen\n"})
	want(t, c, 201, "")
	done := g.Call(t, "POST", "/v1/menu-imports/"+c.JSON(t)["id"].(string)+"/commit", owner, obj{"row_count": 1})
	want(t, done, 200, "")
	if done.JSON(t)["status"] != "committed" || len(g.Emitted()) != 0 {
		t.Fatalf("a queued emission is accepted while the gateway is down and waits: %s", done.Body)
	}
	if body := g.Call(t, "GET", "/v1/items?q=soup", owner, nil).Body; strings.Contains(string(body), "Soup") {
		t.Fatalf("the item appears only after its op returns on the bus: %s", body)
	}

	g.SetGatewayDown(false)
	g.Settle(t)
	ops := g.Emitted()
	if len(ops) != 2 || ops[0].Muts[0].E != "category" || ops[1].Muts[0].E != "item" || ops[0].Seq != 1 || ops[1].Seq != 2 {
		t.Fatalf("the queue drains in order once the gateway is back: %+v", ops)
	}
	if body := g.Call(t, "GET", "/v1/items?q=soup", owner, nil).Body; !strings.Contains(string(body), "Soup") {
		t.Fatalf("the imported item is projected: %s", body)
	}
	g.Settle(t)
	if len(g.Emitted()) != 2 {
		t.Fatal("a drained queue sends nothing again")
	}

	first := g.Call(t, "PUT", itemURL+"/tax-class", keyed, tax)
	second := g.Call(t, "PUT", itemURL+"/tax-class", keyed, tax)
	want(t, first, 200, "")
	if string(first.Body) != string(second.Body) || len(g.Emitted()) != 3 {
		t.Fatalf("the retry with the same Idempotency-Key lands once: %d ops", len(g.Emitted()))
	}
}

// ImportBrand covers which brand an import targets: brand_id, else the brand of X-Outlet-Id, else the only
// brand, else 422 brand_required; a brand that is not one of the caller's is 404.
func ImportBrand(t *testing.T, mk Factory) {
	g := mk(t)
	w := seed.TwoBrands
	g.Load(t, w, seed.Base)
	g.Load(t, seed.Example, seed.Base)
	owner := g.Owner(w.Org())
	const csv = "category,item name,item code,variant name,price,veg mark,station\nMains,Soup,401,,99.00,veg,Kitchen\n"
	create := func(h http.Header, brand any) itest.Response {
		body := obj{"filename": "menu.csv", "csv_text": csv}
		if brand != nil {
			body["brand_id"] = brand
		}
		return g.Call(t, "POST", "/v1/menu-imports", h, body)
	}
	commit := func(r itest.Response) string {
		want(t, r, 201, "")
		want(t, g.Call(t, "POST", "/v1/menu-imports/"+r.JSON(t)["id"].(string)+"/commit", owner, obj{"row_count": 1}), 200, "")
		ops := g.Emitted()
		return ops[len(ops)-1].Stream
	}

	want(t, create(owner, nil), 422, "brand_required")
	if n := len(g.Emitted()); n != 0 {
		t.Fatalf("a refused preview emits nothing: %d", n)
	}
	if got := commit(create(owner, w.Brand2().String())); got != "brand:"+w.Brand2().String() {
		t.Fatalf("brand_id names the brand: %s", got)
	}
	if got := commit(create(outletHeader(owner, w.Outlet()), nil)); got != "brand:"+w.Brand().String() {
		t.Fatalf("the brand of the outlet in X-Outlet-Id: %s", got)
	}
	want(t, create(owner, seed.Example.Brand().String()), 404, "not_found") // another organisation's brand
	want(t, create(outletHeader(owner, w.OutletY()), w.Brand().String()), 404, "not_found")
	want(t, create(outletHeader(owner, seed.Example.Outlet()), nil), 404, "not_found")
	if only := create(g.Owner(seed.Example.Org()), nil); only.Status != 201 {
		t.Fatalf("an organisation with one brand needs no brand_id: %d %s", only.Status, only.Body)
	}
}
