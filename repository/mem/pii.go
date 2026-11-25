package mem

import (
	"context"

	gsync "github.com/hostryhq/go-common/sync"
)

// Catalog owns no pii entity (see the Postgres store), so these are no-ops.

func (memSync) PutPII(context.Context, gsync.PIIWrite) error { return nil }

func (memSync) AddErasure(context.Context, string, gsync.OpRef) (int64, error) { return 0, nil }

func (memSync) Erased(context.Context, string, gsync.OpRef) (bool, error) { return false, nil }
