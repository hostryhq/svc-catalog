package uuid5

import "testing"

// RFC 9562 appendix: UUIDv5 of "www.example.com" in the DNS namespace.
func TestKnownVector(t *testing.T) {
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := Of(dns, "www.example.com"); got != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Fatalf("got %s", got)
	}
}

func TestPriceEntryIsDeterministicPerItemAndVariant(t *testing.T) {
	a, b := PriceEntry("i1", "v1"), PriceEntry("i1", "v1")
	if a != b || a == PriceEntry("i1", "v2") || PriceEntry("i1", "") != PriceEntry("i1", "-") {
		t.Fatal("price_entry id must be a pure function of (item, variant or -)")
	}
}
