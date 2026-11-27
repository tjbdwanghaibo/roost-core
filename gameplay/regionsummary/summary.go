// Package regionsummary publishes an optional, map-wide count of explicitly public objects in
// disjoint business-defined rectangles. The map's own Interest and Manager deliver it; this
// is not another AOI, an authority, or a replacement for any per-object view.
package regionsummary

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const Namespace = "region_count"

// Area names a public map marker. Reserve its SubjectID across the entire Manager (not just
// this map); clients use the same static id-to-rectangle map. Never use a contributor's ID.
type Area struct {
	SubjectID int64
	Bounds    spatial.Rect
}

type Config struct {
	View, DetailView policy.ViewID      // reserve two distinct relations on this map
	DetailProfile    entity.SyncProfile // explicitly public, already supported by each selected source's packer; never the default/self profile
	Areas            []Area             // 1..64 non-overlapping, half-open rectangles; gaps do not contribute
	// Public declares that the object's presence may count for ANY authorized overview
	// observer of this map. Use immutable business rules, not a live DAO or an AOI test.
	Public func(id int64) bool
}

func (c Config) Clone() Config { c.Areas = slices.Clone(c.Areas); return c }

func (c Config) validate(bounds spatial.Rect) error {
	key := c.DetailProfile.Normalize().Key
	if c.Public == nil || c.View == 0 || c.DetailView == 0 || c.DetailView == c.View || key == "default" || key == policy.SelfProfile.Key || strings.Contains(key, "+") || len(c.Areas) == 0 || len(c.Areas) > 64 {
		return errors.New("regionsummary: need two reserved views, a named public detail profile, public filter and 1..64 areas")
	}
	for i, a := range c.Areas {
		r := a.Bounds
		if a.SubjectID <= 0 || r.Min.X >= r.Max.X || r.Min.Y >= r.Max.Y || r.Min.X < bounds.Min.X || r.Min.Y < bounds.Min.Y || r.Max.X > bounds.Max.X || r.Max.Y > bounds.Max.Y {
			return fmt.Errorf("regionsummary: invalid subject or bounds for area %d", i)
		}
		for _, b := range c.Areas[:i] {
			if a.SubjectID == b.SubjectID || (r.Min.X < b.Bounds.Max.X && b.Bounds.Min.X < r.Max.X && r.Min.Y < b.Bounds.Max.Y && b.Bounds.Min.Y < r.Max.Y) {
				return fmt.Errorf("regionsummary: repeated ID or overlapping area %d", i)
			}
		}
	}
	return nil
}

type bucket struct {
	area      Area
	count     uint64        // under Map.mu; zero is represented by Remove, never a public zero update
	published atomic.Uint64 // last nonzero count, read by the packer without the map lock
	state     *entity.SubjectSyncState
}

// Full and delta contain the same absolute count in a standard protobuf UInt64Value;
// no source roster, positions, private fields, or custom frame / delta format.
func (b *bucket) pack() (entity.FrozenSyncPayload, error) {
	p, err := proto.Marshal(wrapperspb.UInt64(b.published.Load()))
	return entity.TakeFrozenSyncPayload(1, p), err
}

type Map struct {
	mu           sync.Mutex
	interest     *policy.Interest
	manager      *entitysync.Manager
	view, detail policy.ViewID
	public       func(int64) bool
	buckets      []*bucket
	markers      map[int64]bool // immutable
	sources      map[int64]*bucket
	closed       bool
	cause        error // broken ownership contract: projection fails closed, ordinary AOI stays live (OrdinarySeen falls back to Seen)
}

// New installs two empty relations on an existing Interest. The caller owns the Interest and
// Manager, has reserved the IDs, and must close the Map before closing either. No subscribers
// are created until SetMode(Detail or Count); no aggregate is shown through the ordinary spatial view.
func New(manager *entitysync.Manager, interest *policy.Interest, bounds spatial.Rect, c Config) (*Map, error) {
	if manager == nil || interest == nil {
		return nil, errors.New("regionsummary: missing map manager or interest")
	}
	if err := c.validate(bounds); err != nil {
		return nil, err
	}
	if err := interest.DefineRelationView(c.View, entity.SyncProfile{}); err != nil {
		return nil, err
	}
	if err := interest.DefineRelationView(c.DetailView, c.DetailProfile); err != nil {
		_ = interest.RemoveView(c.View)
		return nil, err
	}
	m := &Map{manager: manager, interest: interest, view: c.View, detail: c.DetailView, public: c.Public, sources: make(map[int64]*bucket), markers: make(map[int64]bool)}
	for _, a := range c.Areas {
		b := &bucket{area: a}
		b.state = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: a.SubjectID, Namespace: Namespace, Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) { return b.pack() },
			Delta:    func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) { return b.pack() },
		}})
		if err := manager.Register(b.state); err != nil {
			m.Close()
			return nil, fmt.Errorf("regionsummary: register marker %d: %w", a.SubjectID, err)
		}
		m.buckets = append(m.buckets, b)
		m.markers[a.SubjectID] = true
	}
	return m, nil
}

// Mode affects only this map's two dedicated region subscriptions, not its ordinary AOI.
type Mode uint8

const (
	Off    Mode = iota // neither: only independent sources (near, self, team, ...) remain
	Detail             // individual approved subjects in the configured areas, using DetailProfile
	Count              // same population, one UInt64Value for each nonempty area
)

// SetMode selects the granted map layer. Call after authentication/commit for a current
// observer. The two views swap under the Interest registry lock; refusal keeps the old grant.
// Leave/Unwatch detaches them. A new map admission must grant the intended mode again.
func (m *Map) SetMode(observer int64, mode Mode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		if m.cause != nil {
			return m.cause
		}
		return errors.New("regionsummary: closed")
	}
	var selected policy.ViewID
	switch mode {
	case Off:
	case Detail:
		selected = m.detail
	case Count:
		selected = m.view
	default:
		return errors.New("regionsummary: unknown mode")
	}
	return m.interest.SelectRelationView(observer, m.detail, m.view, selected)
}

// OrdinarySeen preserves all sources except the two region-map layers for callers of
// the original map query: a landmark also held by near/self/team remains in its result.
func (m *Map) OrdinarySeen(observer int64) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		// 宿主违反所有权契约而失败关闭（cause 非空）时，closeLocked 已撤下两个专用层，
		// 普通 AOI 仍活着：退回整张 Interest 的查询，场景的 Nearby（附近聊天、AI 候选）不能整图变空。
		// 宿主自己 Close 后仍返回空：这两个 ViewID 可能已被同号重定义，旧句柄不能把它们当普通来源。
		if m.cause != nil {
			return m.interest.Seen(observer)
		}
		return nil
	}
	return m.interest.SeenWithoutViews(observer, m.detail, m.view)
}

// Place records an already committed enter/move. Repeated placement in a cell is silent.
func (m *Map) Place(id int64, at spatial.Point) {
	if id == 0 || m.markers[id] || !m.public(id) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if current := m.sources[id]; current != nil && current.area.Bounds.Contains(at) {
		return
	}
	var to *bucket
	for _, b := range m.buckets {
		if b.area.Bounds.Contains(at) {
			to = b
			break
		}
	}
	m.change(id, to)
}

// Forget retracts an already committed departure, including destruction, without reading a DAO.
func (m *Map) Forget(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.change(id, nil)
	}
}

func (m *Map) change(id int64, to *bucket) {
	from := m.sources[id]
	if from == to {
		return
	}
	// Only an outside/inside transition changes the detail membership. Crossing between
	// two counted areas does not recreate the individual object for a detail observer.
	if from == nil {
		if !m.keepView(m.interest.AddViewMembers(m.detail, id)) {
			return
		}
	} else if to == nil {
		if !m.keepView(m.interest.RemoveViewMembers(m.detail, id)) {
			return
		}
	}
	changed := false
	if from != nil {
		from.count--
		changed = from.count == 0
		if !changed {
			from.published.Store(from.count)
			from.state.MarkDirty(1)
		}
	}
	if to == nil {
		delete(m.sources, id)
	} else {
		m.sources[id] = to
		to.count++
		changed = to.count == 1 || changed
		to.published.Store(to.count)
		to.state.MarkDirty(1)
	}
	if changed {
		ids := make([]int64, 0, len(m.buckets))
		for _, b := range m.buckets {
			if b.count != 0 {
				ids = append(ids, b.area.SubjectID)
			}
		}
		// The relation is private to this Map. New validated it, Close is serialized by mu,
		// and there is no per-view member limit. An error means the owner broke that contract.
		m.keepView(m.interest.SetViewMembers(m.view, ids))
	}
}

// A caller that prematurely closes the Interest must not crash a committed movement callback.
func (m *Map) keepView(err error) bool {
	if err == nil {
		return true
	}
	m.cause = fmt.Errorf("regionsummary: owned relation unavailable: %w", err)
	m.closeLocked()
	return false
}

func (m *Map) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeLocked()
	// 宿主显式关闭：不再沿用失败关闭时的普通查询退路（见 OrdinarySeen）。
	m.cause = nil
}

func (m *Map) closeLocked() {
	if m.closed {
		return
	}
	m.closed = true
	_ = m.interest.RemoveView(m.view)
	_ = m.interest.RemoveView(m.detail)
	for _, b := range m.buckets {
		m.manager.RetractSyncSubject(b.state)
	}
	m.sources = nil
}
