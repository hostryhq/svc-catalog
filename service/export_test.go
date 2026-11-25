package service

import (
	"context"

	"github.com/hostryhq/svc-catalog/internal/model"
)

// CreateSupportImport previews csv as a support act-mode session would: the import's source is the support
// session's, whatever credential the test context carries (act mode is not reachable through the HTTP edge until
// security-reviewer signs catalog.import).
func (s *Service) CreateSupportImport(ctx context.Context, csv string) (model.Import, error) {
	return s.createMenuImport(ctx, model.SourceSupport, "menu.csv", csv, "", "")
}
