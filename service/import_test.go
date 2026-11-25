package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/apperr"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/internal/gen"
	"github.com/hostryhq/svc-catalog/internal/model"
	"github.com/hostryhq/svc-catalog/internal/seed"
)

const csvHeader = "category,item name,item code,variant name,price,veg mark,station\n"

// A support act-mode session may only add (ADR-0005 A2.5 item 5, security review): a row that would change an
// existing item, variant or price is 422 owner_commit_required and stores nothing; the owner may import the
// same file. The act-mode credential is not accepted at the HTTP edge until the flag is signed, so the rule is
// proved at the service, with the import's source set as a support session sets it.
func TestSupportImportMayOnlyAdd(t *testing.T) {
	svc := newMem(t)
	w := seed.Example
	if err := w.Apply(context.Background(), svc, seed.Base); err != nil {
		t.Fatal(err)
	}
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	ctx := authtest.NewTenantIssuer(testkit.NewClock()).ContextFor(context.Background(), cat, authtest.Spec{Type: auth.TypeUser, Tenant: w.Org(), Grants: authtest.OwnerGrants()})
	jobs := func() int {
		p, err := svc.ListMenuImports(ctx, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(p.Items)
	}

	for name, csv := range map[string]string{
		"a changed price":                  csvHeader + "Starters,Paneer Tikka,101,Full,250.00,veg,Kitchen\n",
		"a renamed item":                   csvHeader + "Starters,Paneer Tikka Special,101,Full,240.00,veg,Kitchen\n",
		"an added row and a changed price": csvHeader + "Mains,Soup,401,,99.00,veg,Kitchen\nStarters,Paneer Tikka,101,Full,250.00,veg,Kitchen\n",
	} {
		_, err := svc.CreateSupportImport(ctx, csv)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != "owner_commit_required" || ae.Kind != apperr.Validation {
			t.Fatalf("%s: want 422 owner_commit_required, got %v", name, err)
		}
	}
	if n := jobs(); n != 0 {
		t.Fatalf("a refused support import stores no job, have %d", n)
	}

	for name, csv := range map[string]string{
		"a new item in a new category":      csvHeader + "Mains,Soup,401,,99.00,veg,Kitchen\n",
		"a new variant of an existing item": csvHeader + "Starters,Paneer Tikka,101,Half,140.00,veg,Kitchen\n",
		"the same price again":              csvHeader + "Starters,Paneer Tikka,101,Full,240.00,veg,Kitchen\n",
	} {
		im, err := svc.CreateSupportImport(ctx, csv)
		if err != nil || im.Source != model.SourceSupport {
			t.Fatalf("%s: a support import that only adds is previewed as a support session: %+v %v", name, im, err)
		}
	}
	if _, err := svc.CreateMenuImport(ctx, "menu.csv", csvHeader+"Starters,Paneer Tikka,101,Full,250.00,veg,Kitchen\n", "", ""); err != nil {
		t.Fatalf("the owner may change prices: %v", err)
	}
}

// A support import is add-only at commit too: a price the owner changed after the preview must not be written
// back by the support commit (security review P2-M4). The commit re-checks against the current state.
func TestSupportImportCommitIsAddOnly(t *testing.T) {
	svc := newMem(t)
	w := seed.Example
	if err := w.Apply(context.Background(), svc, seed.Base); err != nil {
		t.Fatal(err)
	}
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	ctx := authtest.NewTenantIssuer(testkit.NewClock()).ContextFor(context.Background(), cat, authtest.Spec{Type: auth.TypeUser, Tenant: w.Org(), Grants: authtest.OwnerGrants()})
	// The same price again previews as an add-only support import.
	im, err := svc.CreateSupportImport(ctx, csvHeader+"Starters,Paneer Tikka,101,Full,240.00,veg,Kitchen\n")
	if err != nil {
		t.Fatal(err)
	}
	// An unchanged menu commits.
	ok, err := svc.CreateSupportImport(ctx, csvHeader+"Starters,Paneer Tikka,101,Full,240.00,veg,Kitchen\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CommitMenuImport(ctx, ok.ID, ok.RowCount); err != nil {
		t.Fatalf("an add-only support import commits on an unchanged menu: %v", err)
	}
	// The owner changes the price after the preview.
	op := &gsync.Op{Ref: gsync.OpRef{Origin: "owner-device", Stream: "brand:" + w.Brand().String(), Seq: 1}, HLC: "00000000ffff0000", Intent: "menu.item.edit", IntentV: 1,
		TenantID: w.Org(), Muts: []gsync.Mut{{E: "price_entry", ID: w.PriceRow().String(), K: gsync.KindSet, F: map[string]any{"price_paise": int64(26000)}}}}
	if err := svc.ApplyOpTx(ctx, op); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CommitMenuImport(ctx, im.ID, im.RowCount)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "owner_commit_required" || ae.Kind != apperr.Validation {
		t.Fatalf("want 422 owner_commit_required at commit, got %v", err)
	}
}
