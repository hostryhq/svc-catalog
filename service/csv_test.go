package service

import "testing"

func TestPriceToPaiseIsExactAndStrict(t *testing.T) {
	for in, want := range map[string]int64{"240": 24000, "240.00": 24000, "0.5": 50, "75.50": 7550, "0": 0, "1.05": 105, "999999999.99": 99999999999} {
		if got, ok := priceToPaise(in); !ok || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", "-1", "1.234", "1,50", "1e3", " 1", ".5", "1.", "1000000000"} {
		if _, ok := priceToPaise(in); ok {
			t.Errorf("%q must be refused", in)
		}
	}
}

func TestParseCSVFileLevelRules(t *testing.T) {
	if rows, err := parseCSV("\xef\xbb\xbfCategory, Item Name ,PRICE\nA,B,1\n\nC,D,2\n"); err != nil || len(rows) != 2 || rows[1].n != 2 || rows[1].name != "D" {
		t.Fatalf("BOM, case-insensitive header, blank lines: %v %+v", err, rows)
	}
	for name, in := range map[string]string{"empty": "", "header only": "category,item name,price\n", "no price column": "category,item name\nA,B\n", "not utf8": "category,item name,price\nA,\xff,1\n"} {
		if _, err := parseCSV(in); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestRowErrorsAreStableCodes(t *testing.T) {
	ok := csvRow{n: 1, category: "A", name: "B", price: "1", veg: "non-veg"}
	if e := rowErrors(ok); len(e) != 0 {
		t.Fatalf("a valid row has no errors: %v", e)
	}
	bad := csvRow{n: 2, price: "1.999", veg: "yes", code: "x1"}
	got := map[string]string{}
	for _, e := range rowErrors(bad) {
		got[e.Field] = e.Code
	}
	for f, c := range map[string]string{"category": "required", "item name": "required", "price": "price_invalid", "veg mark": "veg_mark_invalid", "item code": "code_invalid"} {
		if got[f] != c {
			t.Errorf("field %s: got %q want %q", f, got[f], c)
		}
	}
}
