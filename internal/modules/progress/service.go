package progress

import (
	"context"
	"database/sql"
	"sync/atomic"

	"memorize/internal/inference/router"
	"memorize/internal/infra/eventbus"
	"memorize/internal/modules/memory"
)

type Service struct {
	mem                *memory.Service
	db                 *sql.DB
	dbPath             string
	router             *router.Router
	bus                *eventbus.Bus
	lastRetentionSweep atomic.Int64
}

func New(mem *memory.Service) *Service {
	var (
		db     *sql.DB
		dbPath string
		rtr    *router.Router
		bus    *eventbus.Bus
	)
	if mem != nil {
		db = mem.DB()
		dbPath = mem.DBPath()
		rtr = mem.Router()
		bus = mem.Bus()
	}
	if bus == nil {
		bus = eventbus.New()
	}
	s := &Service{
		mem:    mem,
		db:     db,
		dbPath: dbPath,
		router: rtr,
		bus:    bus,
	}
	if mem != nil {
		mem.AddMutationListener(func(ctx context.Context, ev memory.MutationEvent) {
			s.RecordMemoryMutationProgress(ctx, ev.Scope, ev.Category, ev.SubCategory, ev.ActionKind, ev.MemoryID, ev.TargetIDs, ev.Content)
		})
	}
	return s
}

func (s *Service) notifyProgressChange() {
	if s != nil && s.bus != nil {
		s.bus.Publish("progress")
	}
}
