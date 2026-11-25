package proj

import (
	"testing"
)

func st(h uint64, origin string, seq int64) Stamp { return Stamp{HLC: h, Origin: origin, Seq: seq} }

func fx(op string, s Stamp, kind string, f map[string]any, prev map[string]string) Effect {
	return Effect{Op: op, St: s, Kind: kind, F: f, Prev: prev}
}

// Applying the same effects in any order gives the same row (ADR-0002 S5): the row is a function of the set.
func TestDeriveIsOrderIndependent(t *testing.T) {
	a := fx("a", st(1, "d1", 1), KInsert, map[string]any{"name_en": "Tea", "sort_order": int64(1)}, nil)
	b := fx("b", st(3, "d2", 1), KSet, map[string]any{"name_en": "Chai"}, nil)
	c := fx("c", st(2, "d3", 1), KSet, map[string]any{"sort_order": int64(9), "name_en": "Masala"}, nil)
	perms := [][]Effect{{a, b, c}, {c, b, a}, {b, a, c}, {c, a, b}}
	var first Derived
	for i, p := range perms {
		d := Derive(p)
		if !d.Exists {
			t.Fatal("row should exist once an insert was applied")
		}
		if i == 0 {
			first = d
			continue
		}
		if d.Fields["name_en"] != first.Fields["name_en"] || d.Fields["sort_order"] != first.Fields["sort_order"] {
			t.Fatalf("order %d changed the row: %v vs %v", i, d.Fields, first.Fields)
		}
	}
	if first.Fields["name_en"] != "Chai" || first.Fields["sort_order"] != int64(9) {
		t.Fatalf("later stamp must win per field, got %v", first.Fields)
	}
	if first.Stamps["name_en"] != FormatHLC(3) {
		t.Fatalf("winner stamp not recorded: %v", first.Stamps)
	}
}

func TestDeriveTieBreaksOnOriginThenSeq(t *testing.T) {
	x := fx("x", st(5, "a-device", 1), KInsert, map[string]any{"name": "X"}, nil)
	y := fx("y", st(5, "b-device", 1), KSet, map[string]any{"name": "Y"}, nil)
	if got := Derive([]Effect{x, y}).Fields["name"]; got != "Y" {
		t.Fatalf("equal hlc: higher origin wins, got %v", got)
	}
}

func TestDeriveRowNeedsAnInsertAndTombstonesFollowTheLatest(t *testing.T) {
	set := fx("s", st(2, "d", 2), KSet, map[string]any{"name_en": "x"}, nil)
	if Derive([]Effect{set}).Exists {
		t.Fatal("a set that arrived before its insert must not create the row")
	}
	ins := fx("i", st(1, "d", 1), KInsert, map[string]any{"name_en": "a"}, nil)
	del := fx("d", st(3, "d", 3), KDel, nil, nil)
	undel := fx("u", st(4, "e", 1), KUndel, nil, nil)
	if d := Derive([]Effect{ins, del}); !d.Exists || !d.Gone {
		t.Fatalf("deleted row must be a tombstone: %+v", d)
	}
	if d := Derive([]Effect{ins, del, undel}); d.Gone {
		t.Fatal("a later undel must restore the row")
	}
	if d := Derive([]Effect{ins, undel, del}); d.Gone {
		t.Fatal("stamp order, not arrival order, decides: undel (4) is after del (3)")
	}
}

func TestConflictsFindConcurrentPriceEdits(t *testing.T) {
	ins := fx("i", st(1, "d1", 1), KInsert, map[string]any{"price_paise": int64(24000)}, nil)
	// d1 and d2 both saw stamp 1 and edit it concurrently.
	e1 := fx("e1", st(2, "d1", 2), KSet, map[string]any{"price_paise": int64(24500)}, map[string]string{"price_paise": FormatHLC(1)})
	e2 := fx("e2", st(3, "d2", 1), KSet, map[string]any{"price_paise": int64(25000)}, map[string]string{"price_paise": FormatHLC(1)})
	got := Conflicts("price_entry", []Effect{e2, ins, e1})
	if len(got) != 1 {
		t.Fatalf("want one conflict (e2 did not see e1), got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.Later.Op != "e2" || c.Earlier.Op != "e1" || c.Superseded {
		t.Fatalf("wrong pair: %+v", c)
	}
	if kept, lost := c.Later.F["price_paise"], c.Earlier.F["price_paise"]; kept != int64(25000) || lost != int64(24500) {
		t.Fatalf("kept %v lost %v", kept, lost)
	}
}

func TestConflictsAreQuietForSequentialEditsAndClosedByASeeingEdit(t *testing.T) {
	ins := fx("i", st(1, "d1", 1), KInsert, map[string]any{"price_paise": int64(100)}, nil)
	e1 := fx("e1", st(2, "d1", 2), KSet, map[string]any{"price_paise": int64(110)}, map[string]string{"price_paise": FormatHLC(1)})
	if got := Conflicts("price_entry", []Effect{ins, e1}); len(got) != 0 {
		t.Fatalf("an edit that saw the previous value is not a conflict: %+v", got)
	}
	e2 := fx("e2", st(3, "d2", 1), KSet, map[string]any{"price_paise": int64(120)}, map[string]string{"price_paise": FormatHLC(1)})
	e3 := fx("e3", st(4, "d3", 1), KSet, map[string]any{"price_paise": int64(130)}, map[string]string{"price_paise": FormatHLC(3)})
	got := Conflicts("price_entry", []Effect{ins, e1, e2, e3})
	if len(got) != 1 || !got[0].Superseded {
		t.Fatalf("a later edit that saw e2 supersedes the conflict: %+v", got)
	}
	if Conflicts("floor", []Effect{ins, e1, e2}) != nil {
		t.Fatal("an entity without review fields has no conflicts")
	}
}

func TestParseBodyAcceptsIntegersAndRefusesFractions(t *testing.T) {
	body := `{"origin":"dev-1","stream":"brand:b","seq":3,"hlc":"0000000000010001","intent":"menu.item.edit","muts":[{"e":"item","id":"i1","k":"set","f":{"name_en":"Tea","sort_order":4,"veg":true},"prev":{"tax_class":"0000000000000001"}}]}`
	op, err := ParseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if op.Key() != "dev-1/brand:b/3" || op.HLC != 0x10001 || op.Muts[0].F["sort_order"] != int64(4) || op.Muts[0].Prev["tax_class"] == "" {
		t.Fatalf("decoded op wrong: %+v", op)
	}
	for name, bad := range map[string]string{
		"fraction":  `{"origin":"o","stream":"s","seq":1,"hlc":"0000000000000001","intent":"x","muts":[{"e":"item","id":"i","k":"set","f":{"price_paise":1.5}}]}`,
		"no origin": `{"stream":"s","seq":1,"hlc":"0000000000000001","intent":"x"}`,
		"bad clock": `{"origin":"o","stream":"s","seq":1,"hlc":"zz","intent":"x"}`,
		"not json":  `nope`,
		"seq zero":  `{"origin":"o","stream":"s","seq":0,"hlc":"0000000000000001","intent":"x"}`,
	} {
		if _, err := ParseBody(bad); err == nil {
			t.Errorf("%s: body must be refused", name)
		}
	}
}

func TestEveryEntityDeclaresItsStream(t *testing.T) {
	if len(Entities) != 17 {
		t.Fatalf("catalog owns 17 entities (api-specs entities/README.md), have %d", len(Entities))
	}
	for name, e := range Entities {
		if e.Stream != "brand" && e.Stream != "outlet" {
			t.Errorf("%s: stream %q", name, e.Stream)
		}
	}
	if !Entities["price_entry"].PriceFields["price_paise"] || Entities["item"].PriceFields["tax_class"] {
		t.Fatal("price fields are the money review fields only")
	}
}
