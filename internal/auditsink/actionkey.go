// Package auditsink holds what the Postgres and in-memory vendor audit sinks share: the stable action key
// stored in vendor_access_log for an operation id (pattern ^[a-z]+(\.[a-z_]+)+$, as in the other services).
// Catalog is read by support sessions in view mode (menu.read is the only support_viewable permission) and,
// once security-reviewer signs catalog.import, written by act mode.
package auditsink

import (
	"strings"
	"unicode"
)

var keys = map[string]string{
	"getMenu":              "support.read.menu",
	"listItems":            "support.read.menu",
	"getItem":              "support.read.menu",
	"listCategories":       "support.read.menu",
	"listModifierGroups":   "support.read.menu",
	"listReasonCodes":      "support.read.menu",
	"listItemAvailability": "support.read.menu",
	"createMenuImport":     "support.act.menu_import",
	"commitMenuImport":     "support.act.menu_import_commit",
	"cancelMenuImport":     "support.act.menu_import_cancel",
	"getMenuImport":        "support.read.menu_import",
	"listMenuImports":      "support.read.menu_import",
}

// ActionKey returns the stable action key for an operation id.
func ActionKey(operationID string) string {
	if k, ok := keys[operationID]; ok {
		return k
	}
	var b strings.Builder
	b.WriteString("op.")
	for i, r := range operationID {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
