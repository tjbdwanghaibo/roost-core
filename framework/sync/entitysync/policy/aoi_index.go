package policy

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// AOIIndexMode selects the spatial search structure for the lifetime of an AOI.
type AOIIndexMode string

const (
	IndexBlock     AOIIndexMode = "block"
	IndexCrossList AOIIndexMode = "crosslist"
	IndexKDTree    AOIIndexMode = "kdtree"
)

// IDs are unique within one index. appendRect appends the points in the
// half-open box and is safe for concurrent readers after prepare returns.
type aoiPointIndex interface {
	put(id int64, p spatial.Point)
	drop(id int64)
	prepare()
	appendRect(dst []int64, box spatial.Rect) []int64
}

// Writes happen only in the coordinator's write phase. Preparing on demand
// avoids rebuilding for updates handled entirely by evaluatePair/evaluateMoved.
// Several batch readers may discover the first *actual* scan simultaneously.
type aoiPreparedPoints struct {
	backend aoiPointIndex
	mu      sync.Mutex
	ready   atomic.Bool
}

func (p *aoiPreparedPoints) put(id int64, at spatial.Point) {
	p.backend.put(id, at)
	p.ready.Store(false)
}

func (p *aoiPreparedPoints) drop(id int64) {
	p.backend.drop(id)
	p.ready.Store(false)
}

func (p *aoiPreparedPoints) appendRect(dst []int64, box spatial.Rect) []int64 {
	if !p.ready.Load() {
		p.mu.Lock()
		if !p.ready.Load() {
			p.backend.prepare()
			p.ready.Store(true)
		}
		p.mu.Unlock()
	}
	return p.backend.appendRect(dst, box)
}

type aoiPointIndexes struct {
	subjects, observers aoiPreparedPoints
	// Write-phase scratch; never shared with batch reader workspaces.
	scratch []int64
	changes []aoiBlockChange
}

type aoiBlockChange struct {
	block   int64
	subject int64 // zero (not a valid subject ID) denotes explicit coverage
}

// config has already been validated and its zero mode normalized.
func (m *AOI) initIndex(config AOIConfig) error {
	if config.IndexMode == IndexBlock {
		index, err := spatial.NewBlockIndex(config.Bounds, config.BlockSize)
		if err != nil {
			return err
		}
		m.index, m.layout = index, index.Layout()
		return nil
	}
	layout, err := spatial.NewBlockLayout(config.Bounds, config.BlockSize)
	if err != nil {
		return err
	}
	m.layout, m.points = layout, &aoiPointIndexes{}
	switch config.IndexMode {
	case IndexCrossList:
		m.points.subjects.backend, m.points.observers.backend = newAOICrossList(), newAOICrossList()
	case IndexKDTree:
		m.points.subjects.backend, m.points.observers.backend = newAOIKDTree(), newAOIKDTree()
	}
	return nil
}

// Half-open box containing every representable world point at distance <= r
// on each axis. MaxInt64 itself cannot be a point of any half-open AOI bounds.
func aoiRadiusBox(at spatial.Point, r int64) spatial.Rect {
	return spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(at.X, r), Y: spatial.SaturatingSub(at.Y, r)},
		Max: spatial.Point{X: spatial.SaturatingAdd(at.X, r+1), Y: spatial.SaturatingAdd(at.Y, r+1)},
	}
}

func (m *AOI) pointCandidates(o *interestObserver, w *aoiWorkspace) {
	w.candidates = w.candidates[:0]
	if len(m.subjects) == 0 {
		return
	}
	// Current members have already been evaluated against the leave radius. Only
	// potential new entrants remain; there is no reason to scan whole blocks.
	w.query = m.points.subjects.appendRect(w.query[:0], aoiRadiusBox(o.at, o.enter))
	for _, id := range w.query {
		if _, visible := o.visible[id]; visible || id == o.self {
			continue
		}
		at := m.subjects[id]
		if spatial.WithinDistance(o.at, at, o.enter) {
			w.candidates = append(w.candidates, entryCandidate{subject: id, at: at, distance: spatial.DistanceSquared(o.at, at)})
		}
	}
}

// Query observer *centres* by the changed logical block expanded by the
// maximum leave radius. Then filter by each observer's actual subscribed
// blocks: a same-block move outside its circle can still trigger a refill.
// Do not clip this reverse query to the area's bounds: mirrors can be outside.
func (m *AOI) appendPointSubscribers(dst []int64, block int64) []int64 {
	if block < 0 || len(m.observers) == 0 {
		return dst
	}
	box := m.layout.BlockRect(block)
	r := m.config.MaxLeaveRadius
	box.Min.X, box.Min.Y = spatial.SaturatingSub(box.Min.X, r), spatial.SaturatingSub(box.Min.Y, r)
	box.Max.X, box.Max.Y = spatial.SaturatingAdd(box.Max.X, r), spatial.SaturatingAdd(box.Max.Y, r)
	start := len(dst)
	dst = m.points.observers.appendRect(dst, box)
	keep := dst[:start]
	for _, id := range dst[start:] {
		if _, ok := m.observers[id].blocks[block]; ok {
			keep = append(keep, id)
		}
	}
	return keep
}

func (p *aoiPointIndexes) recycle(ids []int64) {
	if cap(ids) > aoiWorkspaceLimit {
		p.scratch = nil
	} else {
		p.scratch = ids[:0]
	}
}

func (m *AOI) pointSubjectChanged(subject int64, at spatial.Point, removed bool, from, to spatial.Point) {
	if len(m.observers) == 0 {
		return
	}
	first, second := m.BlockAt(from), m.BlockAt(to)
	p := m.points
	if m.batching {
		p.changes = append(p.changes, aoiBlockChange{first, subject})
		if second != first {
			p.changes = append(p.changes, aoiBlockChange{second, subject})
		}
		return
	}
	ids := m.appendPointSubscribers(p.scratch[:0], first)
	if second != first {
		ids = m.appendPointSubscribers(ids, second)
	}
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		m.evaluatePair(m.observers[id], subject, at, removed)
	}
	p.recycle(ids)
}

// Reverse lookups can wait for the final observer positions in this batch:
// every observer moved or replaced during the batch already gets a full
// spatial refresh (and a coverage refresh when its subscriptions changed).
// Unmoved observers still receive every changed block, including intermediate
// subject positions. Query once per distinct block after all writes, rather
// than preparing the observer index on each alternating observer/subject move.
func (m *AOI) resolvePointChanges() {
	p := m.points
	changes := p.changes
	slices.SortFunc(changes, func(a, b aoiBlockChange) int {
		if order := cmp.Compare(a.block, b.block); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})
	changes = slices.Compact(changes)
	ids := p.scratch[:0]
	for len(changes) != 0 {
		block := changes[0].block
		n := 1
		for n < len(changes) && changes[n].block == block {
			n++
		}
		ids = m.appendPointSubscribers(ids[:0], block)
		for _, id := range ids {
			o := m.observers[id]
			for _, change := range changes[:n] {
				if change.subject == 0 {
					o.dirty |= dirtyCoverage
					continue
				}
				if o.dirty&dirtySpatial == 0 {
					o.moved = append(o.moved, change.subject)
				}
				o.dirty |= dirtyMoved
			}
			m.dirty[id] = o
		}
		changes = changes[n:]
	}
	p.recycle(ids)
	if cap(p.changes) > aoiWorkspaceLimit {
		p.changes = nil
	} else {
		p.changes = p.changes[:0]
	}
}
