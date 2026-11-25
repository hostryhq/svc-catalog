// Package uuid5 derives the deterministic row ids that two offline devices must agree on (api-specs
// entities/README.md "Provisional choices": namespace 9a3c7f1e-5b2d-5c84-a1e6-4d0f8b27c3a9, to be fixed in
// go-common/sync). Catalog uses it for price_entry and item_modifier_group rows it emits (menu import).
package uuid5

import (
	"crypto/sha1" //nolint:gosec // RFC 9562 name-based UUIDv5 is defined over SHA-1; not used for security
	"encoding/hex"
)

// Namespace is the Hostry synced-entity namespace.
var Namespace = [16]byte{0x9a, 0x3c, 0x7f, 0x1e, 0x5b, 0x2d, 0x5c, 0x84, 0xa1, 0xe6, 0x4d, 0x0f, 0x8b, 0x27, 0xc3, 0xa9}

// New returns the UUIDv5 of name in Namespace, in canonical lower-case form.
func New(name string) string { return Of(Namespace, name) }

// Of returns the UUIDv5 of name in ns.
func Of(ns [16]byte, name string) string {
	h := sha1.New() //nolint:gosec
	h.Write(ns[:])
	h.Write([]byte(name))
	s := h.Sum(nil)
	var u [16]byte
	copy(u[:], s[:16])
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// PriceEntry is the id of the price row of an item (variant "" for an item without variants).
func PriceEntry(itemID, variantID string) string {
	if variantID == "" {
		variantID = "-"
	}
	return New("price_entry|" + itemID + "|" + variantID)
}

// ItemOutlet is the id of the per-outlet overlay row of an item.
func ItemOutlet(outletID, itemID string) string { return New("item_outlet|" + outletID + "|" + itemID) }
