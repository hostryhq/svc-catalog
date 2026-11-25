// Package events builds the domain events catalog publishes (api-specs events/catalog). Events go to the
// transactional outbox in the same transaction as the state change. Payloads carry ids and counts only
// (ADR-0011 B6).
package events

import (
	"encoding/json"
	"time"

	cevents "github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
)

// Source is the publishing service.
const Source = "catalog"

// TypeMenuImportDone is the event published when an import has been committed and its ops emitted.
const TypeMenuImportDone = "catalog.menu_import.committed"

// MenuImportData is the payload of the menu-import event.
type MenuImportData struct {
	ImportID     ids.ID `json:"import_id"`
	Source       string `json:"source"`
	RowCount     int    `json:"row_count"`
	ItemsCreated int    `json:"items_created"`
	ItemsUpdated int    `json:"items_updated"`
	PriceChanges int    `json:"price_changes"`
}

// MenuImportDone builds the import-completed event (key: the import job, key_seq assigned by the outbox).
func MenuImportDone(id ids.ID, at time.Time, actor string, tenant ids.ID, d MenuImportData) cevents.Envelope {
	b, _ := json.Marshal(d)
	return cevents.Envelope{ID: id, Type: TypeMenuImportDone, V: 1, Source: Source, TenantID: tenant, Key: d.ImportID.String(),
		OccurredAt: at.UTC(), Actor: actor, Data: b}
}
