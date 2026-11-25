package service_test

import (
	"log/slog"
	"testing"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-catalog/internal/projsuite"
	"github.com/hostryhq/svc-catalog/internal/seed"
	"github.com/hostryhq/svc-catalog/repository/mem"
	"github.com/hostryhq/svc-catalog/service"
)

func newMem(t *testing.T) *service.Service {
	clk := testkit.NewClock()
	clk.Set(seed.At)
	var c clock.Clock = clk
	return service.New(mem.New(), service.Options{Clock: c, IDs: ids.NewGenerator(clk.Now, nil), Log: slog.New(slog.DiscardHandler)})
}

// The projection suites over the in-memory repository; integration runs the same over Postgres.
func TestProjectionMem(t *testing.T) { projsuite.Run(t, newMem) }
