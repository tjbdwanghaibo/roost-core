package policy

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 远端只读对象（mirror）：内容的权威不在本进程、本进程也没有它的 Nest 实体，但本进程的客户端要看见它——分区
// Scene 里 Base 进程的玩家看见所在格（另一个进程）的 Real 与 Ghost 就是这种对象。Manager 与 Interest 本来就不要求
// subject 是实体（SubjectSyncState 是不含观察者的内容状态机，不绑实体时 packer 直接执行、提交门为空即就绪；
// Interest 的 Show / Move / Hide 只认 ID 与坐标），所以 Mirrors 只是把“谁维护这份远端内容”收在一处：
//
//	Apply（新对象）   新建不绑实体的 SubjectSyncState → Register（同 ID 还在退役就排在退役之后）→ Interest.Show
//	Apply（更新）     版本不新于已接受的丢弃；新的替换内容 → MarkDirty(mask) 或 MarkFullDirty → 位置变了 Interest.Move
//	Touch            上层确认来源仍存活时续活完全相同的内容版本，不标脏、不重建已删除对象
//	Remove / Expire   Interest.Hide → Manager.Unregister（订阅者下一帧收到 remove）→ 关闭状态 → 留墓碑
//
// 与本地实体在同一个 Manager、同一会话流里，客户端看不出区别（同一 namespace / kind / packer 格式由调用方保证）。
// CellWorld 接线见 gameplay/cellworld/README.md。

// ErrMirrorConfig 表示 MirrorConfig 不合法。
var ErrMirrorConfig = errors.New("policy: invalid mirror config")

// ErrMirrorsClosed 表示 Mirrors 已关闭，不再接受内容。
var ErrMirrorsClosed = errors.New("policy: mirrors closed")

// MirrorVersion 是远端对象内容的版本：Epoch 是迁移代际（对象换一个权威进程 +1），Seq 是同一对象跨 epoch 单调的序号。
// 按 (Epoch, Seq) 字典序比较；接收端只接受更新的版本，不依赖传输的投递顺序。
type MirrorVersion struct {
	Epoch, Seq uint64
}

// Newer 报告 v 是否严格新于 old。
func (v MirrorVersion) Newer(old MirrorVersion) bool {
	if v.Epoch != old.Epoch {
		return v.Epoch > old.Epoch
	}
	return v.Seq > old.Seq
}

// MirrorConfig 配置 Mirrors。除 Delta 外都必填。
type MirrorConfig[T any] struct {
	// Manager 承载 mirror subject；与本地实体用同一个 Manager（同一会话流）。
	Manager *entitysync.Manager
	// Interest 决定谁看见 mirror：mirror 是 Show 进 AOI 的目标，位置由 Position 给出。
	Interest *Interest
	// Position 返回内容里的位置；必须在 Interest 的 AOI 范围内，越界的内容被拒绝（Apply 返回错误）。
	Position func(T) spatial.Point
	// Snapshot 把内容编码成某个视图的全量载荷。在 Manager 的捕获路径上调用，内容是那一刻最新已接受的版本；
	// 不得阻塞、不得调用本 Mirrors。
	Snapshot func(T, entity.SyncProfile) (entity.FrozenSyncPayload, error)
	// Delta 可选：把内容按字段掩码编码成增量。mask 是自上次交付以来各次 Apply 的 Mask 之并。为 nil 时只发全量。
	Delta func(T, entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error)
	// TTL：超过它没有成功 Apply / Touch 的 mirror 由 Expire 删除（单份内容可用 MirrorItem.Hold 换成别的保留时间）；墓碑保留
	// 2 × max(TTL, 保留时间)。
	TTL time.Duration
}

// MirrorItem 是一次远端内容。
type MirrorItem[T any] struct {
	ID int64
	// Namespace / Kind 是帧上的路由键与类型，与该对象作为本地实体时相同；只在第一次（新建）时取用。
	Namespace string
	Kind      uint32
	Version   MirrorVersion
	Content   T
	// Mask 是相对上一个被接受版本改了哪些字段；0 表示全量。调用方跳过了中间版本（丢消息）时必须给 0：
	// 增量只在相邻版本之间成立。
	Mask uint64
	// Hold 是这份内容被接受后、没有更新时保留多久（Expire 按它删除）；0 取 MirrorConfig.TTL。同一对象可以有发送周期
	// 不同的来源（分区 Scene 里 witness 每拍一份、队伍快照每秒一份）：保留时间跟着最近一次被接受的那份走。
	Hold time.Duration
}

// MirrorOutcome 是 Apply 的结果。
type MirrorOutcome uint8

const (
	// MirrorCreated：新对象，已登记并显示。
	MirrorCreated MirrorOutcome = iota + 1
	// MirrorUpdated：已替换内容。
	MirrorUpdated
	// MirrorStale：版本不新于已接受的版本，丢弃。
	MirrorStale
	// MirrorTombstoned：对象已删除，版本不新于删除时的版本，丢弃（迟到的旧消息不能让它复活）。
	MirrorTombstoned
)

func (o MirrorOutcome) String() string {
	switch o {
	case MirrorCreated:
		return "created"
	case MirrorUpdated:
		return "updated"
	case MirrorStale:
		return "stale"
	case MirrorTombstoned:
		return "tombstoned"
	}
	return fmt.Sprintf("MirrorOutcome(%d)", uint8(o))
}

// MirrorStats 是累计计数。
type MirrorStats struct {
	Created, Updated, Stale, Tombstoned, Removed, Expired uint64
}

// mirrorEntry 是一个已登记的 mirror。content 是原子指针：packer 在 Manager 的捕获路径上读它，不取 Mirrors 的锁。
type mirrorEntry[T any] struct {
	version MirrorVersion
	content atomic.Pointer[T]
	state   *entity.SubjectSyncState
	at      spatial.Point
	heardAt time.Time
	hold    time.Duration // 最近一次被接受的内容的保留时间（MirrorItem.Hold，0 已换成 TTL）
}

type mirrorTomb struct {
	version MirrorVersion
	until   time.Time
}

// Mirrors 维护一组远端只读 subject。
//
// 并发：mu 保护表，并把同一时刻的全部 Manager / Interest 调用串成一条线（同一对象的登记、显示、移动、退役不交错）。
// 这些调用都只取各自的短锁、不等待 Flush，所以 Apply 可以在 Nest 快池上调用；Manager 在捕获时调用 packer，
// packer 只读 entry.content，不取 mu——mu 内调用 Manager 不会与 packer 互等。
type Mirrors[T any] struct {
	cfg MirrorConfig[T]

	mu      sync.Mutex
	entries map[int64]*mirrorEntry[T]
	tombs   map[int64]mirrorTomb
	closed  bool
	stats   MirrorStats
}

// NewMirrors 校验配置并构造 Mirrors。
func NewMirrors[T any](cfg MirrorConfig[T]) (*Mirrors[T], error) {
	switch {
	case cfg.Manager == nil || cfg.Interest == nil:
		return nil, fmt.Errorf("%w: manager and interest are required", ErrMirrorConfig)
	case cfg.Position == nil || cfg.Snapshot == nil:
		return nil, fmt.Errorf("%w: position and snapshot are required", ErrMirrorConfig)
	case cfg.TTL <= 0:
		return nil, fmt.Errorf("%w: ttl %v must be positive", ErrMirrorConfig, cfg.TTL)
	}
	return &Mirrors[T]{cfg: cfg, entries: make(map[int64]*mirrorEntry[T]), tombs: make(map[int64]mirrorTomb)}, nil
}

// Apply 应用一次远端内容（见类型说明）。错误只出现在参数不合法、位置越界、Mirrors 已关闭或 Manager 拒绝登记时；
// 丢弃旧版本不是错误（MirrorStale / MirrorTombstoned）。
func (m *Mirrors[T]) Apply(item MirrorItem[T], now time.Time) (MirrorOutcome, error) {
	if item.ID == 0 {
		return 0, fmt.Errorf("%w: mirror id 0", ErrInterestUnknown)
	}
	at := m.cfg.Position(item.Content)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrMirrorsClosed
	}
	if e := m.entries[item.ID]; e != nil {
		return m.updateLocked(e, item, at, now)
	}
	if tomb, ok := m.tombs[item.ID]; ok && !item.Version.Newer(tomb.version) {
		m.stats.Tombstoned++
		return MirrorTombstoned, nil
	}
	return m.createLocked(item, at, now)
}

// Touch 续活当前完全相同的版本，返回是否命中。上层须先校验来源和心跳的新鲜度；
// 旧版本、已删除对象与关闭状态不能续活。它不改内容、位置、Hold，不产生客户端更新。
// Apply 的重复包仍然丢弃，不能仅凭重放内容延长存活；业务心跳显式使用本入口。
func (m *Mirrors[T]) Touch(id int64, version MirrorVersion, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if m.closed || e == nil || e.version != version {
		return false
	}
	if now.After(e.heardAt) {
		e.heardAt = now
	}
	return true
}

func (m *Mirrors[T]) createLocked(item MirrorItem[T], at spatial.Point, now time.Time) (MirrorOutcome, error) {
	e := &mirrorEntry[T]{version: item.Version, at: at, heardAt: now, hold: m.holdOf(item)}
	content := item.Content
	e.content.Store(&content)
	e.state = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: item.ID, Namespace: item.Namespace, SubjectKind: item.Kind,
		Packer: m.packer(e),
	})
	// 先登记再显示：Interest 不能把观察者订阅到 Manager 没有的 subject。同 ID 的上一次删除还在发 remove 时，
	// 新状态排在退役完成之后登记（remove-before-create）；这期间 Interest 的订阅被拒、按重试表重说。
	if _, err := m.cfg.Manager.RegisterAfterRetirement(e.state, nil); err != nil {
		e.state.Close()
		return 0, fmt.Errorf("policy: register mirror %d: %w", item.ID, err)
	}
	if err := m.cfg.Interest.Show(item.ID, at); err != nil {
		_ = m.cfg.Manager.Unregister(item.ID)
		e.state.Close()
		return 0, fmt.Errorf("policy: show mirror %d: %w", item.ID, err)
	}
	delete(m.tombs, item.ID)
	m.entries[item.ID] = e
	m.stats.Created++
	return MirrorCreated, nil
}

func (m *Mirrors[T]) updateLocked(e *mirrorEntry[T], item MirrorItem[T], at spatial.Point, now time.Time) (MirrorOutcome, error) {
	if !item.Version.Newer(e.version) {
		m.stats.Stale++
		return MirrorStale, nil
	}
	if at != e.at {
		// 位置先于内容：越界的内容整份拒绝，不留下“内容新、位置旧”的对象。
		if err := m.cfg.Interest.Move(item.ID, at); err != nil {
			return 0, fmt.Errorf("policy: move mirror %d: %w", item.ID, err)
		}
		e.at = at
	}
	content := item.Content
	e.content.Store(&content)
	e.version, e.heardAt, e.hold = item.Version, now, m.holdOf(item)
	if item.Mask == 0 || m.cfg.Delta == nil {
		e.state.MarkFullDirty(entity.SyncFullReasonDirty)
	} else {
		e.state.MarkDirty(item.Mask)
	}
	m.stats.Updated++
	return MirrorUpdated, nil
}

// packer 读 entry 当前内容。Manager 在捕获时调用（不绑实体：没有实体锁，content 是原子读）。
func (m *Mirrors[T]) packer(e *mirrorEntry[T]) entity.SubjectSyncPacker {
	return entity.SubjectSyncPackFunc{
		Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return m.cfg.Snapshot(*e.content.Load(), profile)
		},
		Delta: func(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
			if m.cfg.Delta == nil {
				return m.cfg.Snapshot(*e.content.Load(), profile)
			}
			return m.cfg.Delta(*e.content.Load(), profile, mask)
		},
	}
}

// Remove 显式删除 id（来源说它没了），版本 ver 记进墓碑：之后不新于 ver 的 Apply 被丢弃。ver 不新于当前版本时
// 什么都不做（迟到的删除不能删掉更新的内容）。返回是否删除了。
func (m *Mirrors[T]) Remove(id int64, ver MirrorVersion, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	e := m.entries[id]
	if e == nil {
		if tomb, ok := m.tombs[id]; !ok || ver.Newer(tomb.version) {
			m.tombs[id] = mirrorTomb{version: ver, until: now.Add(2 * m.cfg.TTL)}
		}
		return false
	}
	if ver.Newer(e.version) || ver == e.version {
		m.dropLocked(id, e, ver, now)
		m.stats.Removed++
		return true
	}
	return false
}

// Expire 删除超过保留时间（最近一次被接受的 MirrorItem.Hold，没给时 TTL）没有成功 Apply / Touch 的 mirror，并清理过期墓碑；
// 返回删除的 mirror 数。调用方按自己的节拍调用。
func (m *Mirrors[T]) Expire(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0
	}
	removed := 0
	for id, e := range m.entries {
		if now.Sub(e.heardAt) > e.hold {
			m.dropLocked(id, e, e.version, now)
			removed++
		}
	}
	for id, tomb := range m.tombs {
		if now.After(tomb.until) {
			delete(m.tombs, id)
		}
	}
	m.stats.Expired += uint64(removed)
	return removed
}

// dropLocked 撤掉一个 mirror：先从 AOI 撤（政策释放订阅），再退役 subject（订阅者下一帧收到 remove），最后关闭状态。
func (m *Mirrors[T]) dropLocked(id int64, e *mirrorEntry[T], ver MirrorVersion, now time.Time) {
	_ = m.cfg.Interest.Hide(id)
	_ = m.cfg.Manager.Unregister(id)
	e.state.Close()
	delete(m.entries, id)
	// 墓碑按这个对象的保留时间（至少 TTL）：保留得久的来源发得慢，迟到的旧份也来得晚。
	m.tombs[id] = mirrorTomb{version: ver, until: now.Add(2 * max(m.cfg.TTL, e.hold))}
}

// holdOf 是一份内容被接受后的保留时间：MirrorItem.Hold，没给时取 TTL。
func (m *Mirrors[T]) holdOf(item MirrorItem[T]) time.Duration {
	if item.Hold > 0 {
		return item.Hold
	}
	return m.cfg.TTL
}

// Get 返回 id 当前已接受的内容与版本。
func (m *Mirrors[T]) Get(id int64) (T, MirrorVersion, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil {
		return *e.content.Load(), e.version, true
	}
	var zero T
	return zero, MirrorVersion{}, false
}

// Len 返回当前 mirror 数。
func (m *Mirrors[T]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Stats 返回累计计数。
func (m *Mirrors[T]) Stats() MirrorStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Close 删除全部 mirror（订阅者收到 remove）并拒绝后续 Apply。幂等。Manager / Interest 由调用方关闭。
func (m *Mirrors[T]) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	now := time.Now()
	for id, e := range m.entries {
		m.dropLocked(id, e, e.version, now)
	}
	m.closed = true
}
