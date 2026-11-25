package pg

import (
	"context"

	gsync "github.com/hostryhq/go-common/sync"
)

// Catalog owns no entity with a personal-data field and replicates none, so it holds no pii side table (migration
// 002). An erasure op of another owner therefore has nothing to remove here, and a pii write cannot occur.

// PutPII implements gsync.Store; catalog never owns pii, so there is nothing to store.
func (tenantStore) PutPII(context.Context, gsync.PIIWrite) error { return nil }

// AddErasure implements gsync.Store; with no pii held, zero records are emptied.
func (tenantStore) AddErasure(context.Context, string, gsync.OpRef) (int64, error) { return 0, nil }

// Erased implements gsync.Store; no pii is held, so none is tracked as erased.
func (tenantStore) Erased(context.Context, string, gsync.OpRef) (bool, error) { return false, nil }
