package progress

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"

	"memorize/internal/inference/router"
	"memorize/internal/infra/eventbus"
	"memorize/internal/modules/memory"
)

// Service owns persistent project_progress tracking, Git/memory sync milestones,
// completion retention TTL, and ephemeral in-memory live IDE/MCP sessions.
type Service struct {
	mem                *memory.Service
	db                 *sql.DB
	dbPath             string
	router             *router.Router
	bus                *eventbus.Bus
	mu                 sync.Mutex
	liveSessions       map[string]liveSessionEntry
	lastRetentionSweep atomic.Int64
}

// New constructs a progress Service attached to mem and subscribes to memory mutations
// so committed, updated, and consolidated project:* facts automatically record milestones.
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
		mem:          mem,
		db:           db,
		dbPath:       dbPath,
		router:       rtr,
		bus:          bus,
		liveSessions: make(map[string]liveSessionEntry),
	}
	if mem != nil {
		mem.AddMutationListener(func(ctx context.Context, ev memory.MutationEvent) {
			s.RecordMemoryMutationProgress(ctx, ev.Scope, ev.Category, ev.SubCategory, ev.ActionKind, ev.MemoryID, ev.TargetIDs, ev.Content)
		})
	}
	return s
}

// Memory returns the underlying memory Service.
func (s *Service) Memory() *memory.Service {
	return s.mem
}

func (s *Service) notifyProgressChange() {
	if s != nil && s.bus != nil {
		s.bus.Publish("progress")
	}
}

func (s *Service) notifyLive() {
	if s != nil && s.bus != nil {
		s.bus.PublishLive()
	}
}
