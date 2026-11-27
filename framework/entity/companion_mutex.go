package entity

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// companionDomain counts logical tickets at the stable entrance, NOT by
// reacquiring the native lock. The underlying mutex need not be reentrant.
// depth is only accessed by the owner; native.Unlock/Lock synchronize owners.
type companionDomain struct {
	native lock.Mutex
	owner  atomic.Int64
	depth  int
}

func (d *companionDomain) take(ctx context.Context, try bool) bool {
	gid := goroutine.GoID()
	if d.owner.Load() == gid {
		d.depth++
		return true
	}
	if try {
		if !d.native.TryLock() {
			return false
		}
	} else if ctx == nil {
		d.native.Lock()
	} else if lock.LockContext(ctx, d.native) != nil {
		return false
	}
	d.depth = 1
	d.owner.Store(gid)
	return true
}
func (d *companionDomain) give() {
	if d.owner.Load() != goroutine.GoID() {
		panic("entity: unowned companion domain")
	}
	d.depth--
	if d.depth == 0 {
		d.owner.Store(0)
		d.native.Unlock()
	}
}

// Stable, instance-specific front door; an old reference cannot cache a bare
// mutex. Waiters validate AFTER acquiring the physical lock. tickets counts
// exactly the locks taken via this instance (only accessed under its domain).
type companionGate struct {
	id      int64
	home    *companionDomain
	current atomic.Pointer[companionDomain]
	tickets int
	// Graph fields below are protected by the owning manager's addMu.
	declared     int64
	parent       IThreadSafeEntity
	children     map[int64]IThreadSafeEntity
	epoch        uint64
	pending      *CompanionChange
	reservations int
	retiring     int // fence reentrant lifecycle callbacks while a removal is in progress
}

func newCompanionGate(id int64, native lock.Mutex) *companionGate {
	g := &companionGate{id: id, home: &companionDomain{native: native}}
	g.current.Store(g.home)
	return g
}
func (g *companionGate) acquire(ctx context.Context, try bool) bool {
	for {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		d := g.current.Load()
		if !d.take(ctx, try) {
			return false
		}
		if g.current.Load() == d {
			g.tickets++
			return true
		}
		d.give()
		if try {
			return false
		}
	}
}
func (g *companionGate) Lock()         { g.acquire(nil, false) }
func (g *companionGate) TryLock() bool { return g.acquire(nil, true) }
func (g *companionGate) LockContext(ctx context.Context) error {
	if g.acquire(ctx, false) {
		return nil
	}
	return ctx.Err()
}
func (g *companionGate) LockWithTimeout(t time.Duration) bool {
	if t <= 0 {
		return g.TryLock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	return g.acquire(ctx, false)
}
func (g *companionGate) Unlock() {
	d := g.current.Load()
	if d.owner.Load() != goroutine.GoID() || g.tickets <= 0 {
		panic("entity: unowned companion ticket")
	}
	g.tickets--
	d.give()
}
func (g *companionGate) LockId() int64 { return g.id }

// move transfers outstanding logical tickets while BOTH domains are held.
// Even if a lifecycle change is reentrant inside a framework gate holder,
// that holder's remaining critical section is covered at the new entrance.
func (g *companionGate) move(to *companionDomain) {
	from := g.current.Load()
	if from == to {
		return
	}
	gid := goroutine.GoID()
	if from.owner.Load() != gid || to.owner.Load() != gid {
		panic("entity: change without both physical domains")
	}
	to.depth += g.tickets
	g.current.Store(to)
	for i := 0; i < g.tickets; i++ {
		from.give()
	}
}
func gateOf(e IThreadSafeEntity) *companionGate {
	if e == nil || e.Base() == nil {
		return nil
	}
	g, _ := e.Base().mu.(*companionGate)
	return g
}
