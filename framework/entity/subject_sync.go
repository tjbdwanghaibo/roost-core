package entity

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
)

var (
	ErrSubjectSyncClosed       = errors.New("entity: subject sync state is closed")
	ErrSubjectSyncNotDirty     = errors.New("entity: subject sync state has no pending dirty data")
	ErrSubjectSyncInFlight     = errors.New("entity: subject sync prepare already in flight")
	ErrSubjectSyncPacker       = errors.New("entity: subject sync packer is required")
	ErrSubjectSyncStalePrepare = errors.New("entity: subject sync prepared update is stale")
	ErrSubjectSyncFinished     = errors.New("entity: subject sync prepared update is already finished")
	ErrSubjectSyncBatchInvalid = errors.New("entity: subject sync prepared batch is invalid")
)

const (
	preparedSubjectSyncOpen uint32 = iota
	preparedSubjectSyncFinished
	preparedSubjectSyncReserved
)

const SyncMaskFull uint64 = ^uint64(0)

// subjectSyncInFlightStaleAfter 限定一次未完成的 prepare 能阻塞新 prepare 多久。持有者 panic 或未 Commit / Abort 就丢弃的
// PreparedSubjectSync 否则会永远占着在途令牌，让该 subject 的同步永久停住。回收是安全的：Commit 与 Abort 会校验令牌与
// 基础版本，令牌被取代后它们返回陈旧错误。
const subjectSyncInFlightStaleAfter = 30 * time.Second

const (
	SyncFullReasonNone uint32 = iota
	SyncFullReasonDirty
	SyncFullReasonResync
	SyncFullReasonSchema
)

// SyncProfile 标识一个有限的状态视图，例如 LOD、权限或阵营投影。它有意不包含订阅者身份。
type SyncProfile struct {
	Key           string
	LOD           uint8
	SchemaVersion uint32
}

// Normalize 把空 Key 归为 "default"。
func (p SyncProfile) Normalize() SyncProfile {
	if p.Key == "" {
		p.Key = "default"
	}
	return p
}

// FrozenSyncPayload 持有不可变字节。CopyFrozenSyncPayload 复制调用方内存；TakeFrozenSyncPayload 接管所有权，调用方此后
// 绝不能修改传入的切片。
type FrozenSyncPayload struct {
	codec uint16
	data  []byte
}

func CopyFrozenSyncPayload(codec uint16, data []byte) FrozenSyncPayload {
	return FrozenSyncPayload{codec: codec, data: bytes.Clone(data)}
}

func TakeFrozenSyncPayload(codec uint16, owned []byte) FrozenSyncPayload {
	return FrozenSyncPayload{codec: codec, data: owned}
}

func (p FrozenSyncPayload) Codec() uint16 { return p.codec }

func (p FrozenSyncPayload) Len() int { return len(p.data) }

func (p FrozenSyncPayload) Empty() bool { return len(p.data) == 0 }

func (p FrozenSyncPayload) BytesCopy() []byte { return bytes.Clone(p.data) }

func (p FrozenSyncPayload) AppendTo(dst []byte) []byte { return append(dst, p.data...) }

func (p FrozenSyncPayload) Equal(other FrozenSyncPayload) bool {
	return p.codec == other.codec && bytes.Equal(p.data, other.data)
}

// SubjectSyncPacker 按 profile 序列化实体状态。实现在持有所属实体锁时运行；返回的载荷必须已经用 TakeFrozenSyncPayload
// 转移所有权或显式复制。
type SubjectSyncPacker interface {
	PackSubjectSnapshot(SyncProfile) (FrozenSyncPayload, error)
	PackSubjectDelta(SyncProfile, uint64) (FrozenSyncPayload, error)
}

// SubjectSyncPackFunc 用两个函数实现 SubjectSyncPacker；函数为 nil 时返回空载荷。
type SubjectSyncPackFunc struct {
	Snapshot func(SyncProfile) (FrozenSyncPayload, error)
	Delta    func(SyncProfile, uint64) (FrozenSyncPayload, error)
}

// SubjectSyncDirtyNotifier 由框架调度器安装，只携带内容状态，有意不包含观察者 / 订阅者数据。
type SubjectSyncDirtyNotifier func(*SubjectSyncState)

func (f SubjectSyncPackFunc) PackSubjectSnapshot(profile SyncProfile) (FrozenSyncPayload, error) {
	if f.Snapshot == nil {
		return FrozenSyncPayload{}, nil
	}
	return f.Snapshot(profile.Normalize())
}

func (f SubjectSyncPackFunc) PackSubjectDelta(profile SyncProfile, mask uint64) (FrozenSyncPayload, error) {
	if f.Delta == nil {
		return FrozenSyncPayload{}, nil
	}
	return f.Delta(profile.Normalize(), mask)
}

// SubjectSyncCreateParam 是创建 SubjectSyncState 的参数。
type SubjectSyncCreateParam struct {
	Enabled     bool
	SubjectID   int64
	Namespace   string
	SubjectKind uint32
	Packer      SubjectSyncPacker
}

// EntitySyncCreateParam 是实体同步的唯一配置。状态只包含内容；订阅成员与交付由 entitysync 协调器负责。
type EntitySyncCreateParam struct {
	Enabled  bool
	EntityID int64
	// Namespace 随该 subject 的每次更新上线，是客户端的路由键（"player"、"monster"……）。这是同步层唯一知道的分组：
	// 房间、区域是决定谁订阅的策略，不是帧上的标签。
	Namespace  string
	EntityKind uint32
	Packer     SubjectSyncPacker
}

type EntitySyncBuilderParam struct {
	Enabled       bool
	Namespace     string
	PackerFactory func(IThreadSafeEntity) SubjectSyncPacker
}

func (p EntitySyncBuilderParam) toCreateParam(e IThreadSafeEntity) EntitySyncCreateParam {
	ret := EntitySyncCreateParam{Enabled: p.Enabled, Namespace: p.Namespace}
	if e != nil {
		ret.EntityID = e.ID()
		ret.EntityKind = uint32(e.GetEntityKind())
	}
	if p.PackerFactory != nil {
		ret.Packer = p.PackerFactory(e)
	}
	return ret
}

type SubjectSyncUpdate struct {
	SubjectID   int64
	Namespace   string
	SubjectKind uint32
	Profile     SyncProfile
	Version     uint64
	BaseVersion uint64
	Mask        uint64
	Full        bool
	Reason      uint32
	Payload     FrozenSyncPayload
	// CommitLSN 是捕获载荷那一刻所属实体最新的 pipelined 提交 LSN，在提交路径写入它的同一把实体锁下读取——所以它
	// 恰好对应这份载荷可能包含的提交。交付门把它与持久水位比较；若在捕获之前另读 LastCommitLSN，中间会留下一个
	// 提交落入的窗口。
	CommitLSN uint64
}

type subjectSyncLockFunc func(func() error) error

// SubjectSyncState 是不含观察者的内容状态机。订阅成员、会话序号、路由与历史由 entitysync / wiring 负责。
type SubjectSyncState struct {
	mu        sync.Mutex
	prepareMu sync.Mutex

	enabled         bool
	subjectID       int64
	namespace       string
	subjectKind     uint32
	version         uint64
	dirtyMask       uint64
	fullDirty       bool
	fullReason      uint32
	dirtyGeneration uint64
	packer          SubjectSyncPacker
	lockEntity      subjectSyncLockFunc
	dirtyNotifier   SubjectSyncDirtyNotifier
	// syncOwner 是登记了本状态的同步 Manager（ClaimSyncOwner）。脏通知器、冻结内容与在途捕获在状态上各只有
	// 一份，两个 Manager 同时登记会互相覆盖通知器、作废对方的捕获，所以同一时刻只认一个主人。
	syncOwner  any
	mutation   *syncMutationEntry
	commitGate *syncCommitGate
	// policyHolds 阻止内容越过尚未应用的空间/关系事实，不参与提交确认条件。
	policyHolds     int
	frozen          *frozenSubjectSync
	nextToken       uint64
	inflightToken   uint64
	inflightSince   time.Time
	inflightRelease func()
	lastError       error
	// lastCommitLSN 镜像 EntityBase.lastCommitLSN，订阅协调器不必回引实体就能按持久水位控制分发。原子：在实体锁内写，
	// 由刷新计时器读。
	lastCommitLSN atomic.Uint64
}

func NewSubjectSyncState(param SubjectSyncCreateParam) *SubjectSyncState {
	if !param.Enabled {
		return nil
	}
	return &SubjectSyncState{
		enabled:     true,
		subjectID:   param.SubjectID,
		namespace:   param.Namespace,
		subjectKind: param.SubjectKind,
		packer:      param.Packer,
	}
}

func (s *SubjectSyncState) setEntityLock(lockEntity subjectSyncLockFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lockEntity = lockEntity
	s.mu.Unlock()
}

func (s *SubjectSyncState) withEntityLock(fn func() error) error {
	if fn == nil {
		return nil
	}
	s.mu.Lock()
	lockEntity := s.lockEntity
	s.mu.Unlock()
	if lockEntity == nil {
		return fn()
	}
	return lockEntity(fn)
}

func (s *SubjectSyncState) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *SubjectSyncState) SubjectID() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subjectID
}

func (s *SubjectSyncState) Namespace() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namespace
}

func (s *SubjectSyncState) SubjectKind() uint32 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subjectKind
}

func (s *SubjectSyncState) Version() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *SubjectSyncState) PendingDirty() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirtyMask != 0 || s.fullDirty
}

func (s *SubjectSyncState) LastError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

// ClaimSyncOwner 让 owner（同步 Manager）独占本状态：一个内容状态只喂一个 Manager。本状态没有主人或主人就是
// owner 时记下 owner、返回 (owner, true)；已被另一个主人持有时什么都不改，返回那个主人与 false，由调用方拒绝登记。
// owner 为 nil 时返回 (nil, false)。主人在 ReleaseSyncOwner 之前一直有效（状态 Close 不清除它：关闭的状态不能再登记）。
func (s *SubjectSyncState) ClaimSyncOwner(owner any) (holder any, ok bool) {
	if s == nil || owner == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncOwner != nil && s.syncOwner != owner {
		return s.syncOwner, false
	}
	s.syncOwner = owner
	return owner, true
}

// ReleaseSyncOwner 在 owner 仍是主人时放开本状态，之后别的 Manager 可以登记它；不是 owner 持有时什么都不做
// （迟到的释放不会放开别人的登记）。
func (s *SubjectSyncState) ReleaseSyncOwner(owner any) {
	if s == nil || owner == nil {
		return
	}
	s.mu.Lock()
	if s.syncOwner == owner {
		s.syncOwner = nil
	}
	s.mu.Unlock()
}

func (s *SubjectSyncState) SetDirtyNotifier(notifier SubjectSyncDirtyNotifier) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.enabled {
		s.dirtyNotifier = notifier
	}
	pending := s.enabled && (s.dirtyMask != 0 || s.fullDirty)
	s.mu.Unlock()
	if pending && notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func (s *SubjectSyncState) MarkDirty(mask uint64) {
	if s == nil || mask == 0 {
		return
	}
	s.mu.Lock()
	var notifier SubjectSyncDirtyNotifier
	if s.enabled && s.mutation != nil {
		s.mutation.mask |= mask
		s.mu.Unlock()
		return
	}
	if s.enabled {
		s.dirtyMask |= mask
		s.dirtyGeneration++
		notifier = s.dirtyNotifier
	}
	s.mu.Unlock()
	if notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func (s *SubjectSyncState) MarkFullDirty(reason uint32) {
	if s == nil {
		return
	}
	if reason == SyncFullReasonNone {
		reason = SyncFullReasonDirty
	}
	s.mu.Lock()
	var notifier SubjectSyncDirtyNotifier
	if s.enabled && s.mutation != nil {
		s.mutation.full = true
		s.mutation.reason = reason
		s.mu.Unlock()
		return
	}
	if s.enabled {
		s.fullDirty = true
		s.fullReason = reason
		s.dirtyMask = 0
		s.dirtyGeneration++
		notifier = s.dirtyNotifier
	}
	s.mu.Unlock()
	if notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func notifySubjectSyncDirty(notifier SubjectSyncDirtyNotifier, state *SubjectSyncState) {
	defer func() {
		_ = recover()
	}()
	notifier(state)
}

func entitySyncLockedInCurrentGuard(entityID int64) bool {
	if entityID == 0 {
		return false
	}
	scope := CurrentGuardScope()
	if scope == nil || scope.Guard() == nil {
		return false
	}
	ok := scope.Guard().Guarded(entityID)
	return ok
}

// PrepareViews 捕获明确指定的视图：空列表表示无人需要该类内容。
// 即使没有接收者，也允许提交脏版本；需要默认视图时显式传入 SyncProfile{}。
// 在一次 Entity 锁内捕获全部增量与全量，确保版本、提交水位和内容来自同一时刻。
func (s *SubjectSyncState) PrepareViews(deltaProfiles, snapshotProfiles []SyncProfile) (*PreparedSubjectSync, error) {
	if s == nil {
		return nil, ErrSubjectSyncClosed
	}
	return s.prepareProfiles(NormalizeSyncProfiles(deltaProfiles), NormalizeSyncProfiles(snapshotProfiles))
}

func (s *SubjectSyncState) prepareProfiles(deltaProfiles, snapshotProfiles []SyncProfile) (*PreparedSubjectSync, error) {
	var prepared *PreparedSubjectSync
	err := s.withEntityLock(func() error {
		s.prepareMu.Lock()
		defer s.prepareMu.Unlock()
		var err error
		prepared, err = s.prepareLocked(deltaProfiles, snapshotProfiles)
		return err
	})
	return prepared, err
}

func (s *SubjectSyncState) prepareLocked(deltaProfiles, snapshotProfiles []SyncProfile) (*PreparedSubjectSync, error) {
	s.mu.Lock()
	if s.policyHolds != 0 || !s.commitGate.ready() {
		s.mu.Unlock()
		return nil, ErrSyncCommitPending
	}
	if !s.enabled {
		s.mu.Unlock()
		return nil, ErrSubjectSyncClosed
	}
	if s.inflightToken != 0 {
		if s.inflightSince.IsZero() || time.Since(s.inflightSince) < subjectSyncInFlightStaleAfter {
			s.mu.Unlock()
			return nil, ErrSubjectSyncInFlight
		}
		flog.Error("entity: reclaiming abandoned subject sync prepare",
			"subject", s.subjectID, "namespace", s.namespace,
			"token", s.inflightToken, "age", time.Since(s.inflightSince))
		s.inflightToken = 0
		s.inflightSince = time.Time{}
		if s.inflightRelease != nil {
			s.inflightRelease()
			s.inflightRelease = nil
		}
	}
	dirty := s.dirtyMask != 0 || s.fullDirty
	if !dirty && len(snapshotProfiles) == 0 {
		s.mu.Unlock()
		return nil, ErrSubjectSyncNotDirty
	}
	if s.packer == nil {
		s.lastError = ErrSubjectSyncPacker
		s.mu.Unlock()
		return nil, ErrSubjectSyncPacker
	}
	s.nextToken++
	token := s.nextToken
	s.inflightToken = token
	s.inflightSince = time.Now()
	packer := s.packer
	subjectID := s.subjectID
	namespace := s.namespace
	subjectKind := s.subjectKind
	baseVersion := s.version
	version := baseVersion
	if dirty {
		version++
	}
	mask := s.dirtyMask
	full := s.fullDirty
	reason := s.fullReason
	if full && reason == SyncFullReasonNone {
		reason = SyncFullReasonDirty
	}
	generation := s.dirtyGeneration
	commitLSN := s.lastCommitLSN.Load() // under the entity lock: consistent with the content
	frozen := s.frozen
	s.frozen = nil
	s.mu.Unlock()
	if frozen != nil {
		if item := frozen.prepare(s, token, generation, baseVersion, version, deltaProfiles, snapshotProfiles); item != nil {
			s.mu.Lock()
			s.inflightRelease = frozen.release
			s.mu.Unlock()
			return item, nil
		}
		frozen.release()
	}

	var deltas []SubjectSyncUpdate
	if dirty {
		deltas = make([]SubjectSyncUpdate, 0, len(deltaProfiles))
		for _, profile := range deltaProfiles {
			var payload FrozenSyncPayload
			var err error
			if full {
				payload, err = packer.PackSubjectSnapshot(profile)
			} else {
				payload, err = packer.PackSubjectDelta(profile, mask)
			}
			if err != nil {
				s.failPrepare(token, err)
				return nil, fmt.Errorf("entity: pack subject %d profile %q: %w", subjectID, profile.Key, err)
			}
			deltas = append(deltas, SubjectSyncUpdate{
				SubjectID: subjectID, Namespace: namespace, SubjectKind: subjectKind,
				Profile: profile, Version: version,
				BaseVersion: baseVersion, Mask: mask, Full: full, Reason: reason,
				Payload: payload, CommitLSN: commitLSN,
			})
		}
	}
	snapshots := make([]SubjectSyncUpdate, 0, len(snapshotProfiles))
	for _, profile := range snapshotProfiles {
		var payload FrozenSyncPayload
		var err error
		reused := false
		if full {
			for _, update := range deltas {
				if update.Profile == profile {
					payload, reused = update.Payload, true
					break
				}
			}
		}
		if !reused {
			payload, err = packer.PackSubjectSnapshot(profile)
		}
		if err != nil {
			s.failPrepare(token, err)
			return nil, fmt.Errorf("entity: snapshot subject %d profile %q: %w", subjectID, profile.Key, err)
		}
		snapshots = append(snapshots, SubjectSyncUpdate{
			SubjectID: subjectID, Namespace: namespace, SubjectKind: subjectKind,
			Profile: profile, Version: version, BaseVersion: version,
			Full: true, Reason: SyncFullReasonResync, Payload: payload, CommitLSN: commitLSN,
		})
	}

	return &PreparedSubjectSync{
		state: s, token: token, generation: generation, baseVersion: baseVersion,
		version: version, commitLSN: commitLSN, updates: deltas, snapshots: snapshots,
	}, nil
}

func (s *SubjectSyncState) SetLastCommitLSN(lsn uint64) {
	if s == nil {
		return
	}
	s.lastCommitLSN.Store(lsn)
}

// LastCommitLSN 返回所属实体最新的 pipelined 提交 LSN；没有 pipelined 事务修改过时为 0。它高于 committer 的持久水位时
// 不得分发。
func (s *SubjectSyncState) LastCommitLSN() uint64 {
	if s == nil {
		return 0
	}
	return s.lastCommitLSN.Load()
}

func (s *SubjectSyncState) failPrepare(token uint64, err error) {
	s.mu.Lock()
	if s.inflightToken == token {
		s.inflightToken = 0
		s.inflightSince = time.Time{}
		if s.inflightRelease != nil {
			s.inflightRelease()
			s.inflightRelease = nil
		}
	}
	s.lastError = err
	s.mu.Unlock()
}

func (s *SubjectSyncState) setSubjectSyncError(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastError = err
	s.mu.Unlock()
}

func (s *SubjectSyncState) Close() {
	if s == nil {
		return
	}
	s.prepareMu.Lock()
	s.mu.Lock()
	s.enabled = false
	if s.inflightRelease != nil {
		s.inflightRelease()
		s.inflightRelease = nil
	}
	if s.frozen != nil {
		s.frozen.release()
		s.frozen = nil
	}
	s.dirtyMask = 0
	s.fullDirty = false
	s.fullReason = SyncFullReasonNone
	s.packer = nil
	s.dirtyNotifier = nil
	s.mu.Unlock()
	s.prepareMu.Unlock()
}

type PreparedSubjectSync struct {
	commitLSN   uint64
	state       *SubjectSyncState
	token       uint64
	generation  uint64
	baseVersion uint64
	version     uint64
	updates     []SubjectSyncUpdate // deltas (or full-dirty snapshots) for the subscribers who are up to date
	snapshots   []SubjectSyncUpdate // full snapshots for the subscribers who have nothing yet
	finished    atomic.Uint32
}

// CommitLSN 对应本次内容捕获，包括没有接收者、没有 payload 的捕获。
func (p *PreparedSubjectSync) CommitLSN() uint64 {
	if p == nil {
		return 0
	}
	return p.commitLSN
}

func (p *PreparedSubjectSync) Version() uint64 {
	if p == nil {
		return 0
	}
	return p.version
}

func (p *PreparedSubjectSync) Updates() []SubjectSyncUpdate {
	if p == nil {
		return nil
	}
	return append([]SubjectSyncUpdate(nil), p.updates...)
}

// Snapshots 返回 PrepareViews 为新订阅者做的全量捕获，每个快照 profile 一份，与 Updates 同一版本。
func (p *PreparedSubjectSync) Snapshots() []SubjectSyncUpdate {
	if p == nil {
		return nil
	}
	return append([]SubjectSyncUpdate(nil), p.snapshots...)
}

// BaseVersion 返回捕获时 subject 的版本；捕获不含 delta 时与 Version 相等。
func (p *PreparedSubjectSync) BaseVersion() uint64 {
	if p == nil {
		return 0
	}
	return p.baseVersion
}

func (p *PreparedSubjectSync) Commit() error {
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{p})
	if err != nil {
		return err
	}
	return batch.Commit()
}

func (p *PreparedSubjectSync) Abort() error { return p.AbortWithError(nil) }

func (p *PreparedSubjectSync) AbortWithError(cause error) error {
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{p})
	if err != nil {
		return err
	}
	return batch.AbortWithError(cause)
}

// PreparedSubjectSyncBatch 在下游准入之前预留一组已 prepare 的 subject。预留防止任何调用方在批次在途时单独提交或中止
// 其中一员。Commit 与 AbortWithError 持有全部状态锁更新每个 SubjectSyncState，观察者看不到也造不出部分提交的批次。
type PreparedSubjectSyncBatch struct {
	items    []*PreparedSubjectSync
	finished atomic.Uint32
}

// ReservePreparedSubjectSyncBatch 把每个已 prepare 项的完成所有权转给一个批次。调用方必须在持久准入成功后调用 Commit，
// 失败后调用 AbortWithError。
func ReservePreparedSubjectSyncBatch(items []*PreparedSubjectSync) (*PreparedSubjectSyncBatch, error) {
	if len(items) == 0 {
		return nil, ErrSubjectSyncBatchInvalid
	}
	ordered := append([]*PreparedSubjectSync(nil), items...)
	seen := make(map[*SubjectSyncState]struct{}, len(ordered))
	for _, item := range ordered {
		if item == nil || item.state == nil {
			return nil, ErrSubjectSyncBatchInvalid
		}
		if _, duplicate := seen[item.state]; duplicate {
			return nil, ErrSubjectSyncBatchInvalid
		}
		seen[item.state] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.state.subjectID != right.state.subjectID {
			return left.state.subjectID < right.state.subjectID
		}
		return left.token < right.token
	})
	reserved := 0
	for _, item := range ordered {
		if !item.finished.CompareAndSwap(preparedSubjectSyncOpen, preparedSubjectSyncReserved) {
			for i := 0; i < reserved; i++ {
				ordered[i].finished.CompareAndSwap(preparedSubjectSyncReserved, preparedSubjectSyncOpen)
			}
			return nil, ErrSubjectSyncFinished
		}
		reserved++
	}
	for _, item := range ordered {
		state := item.state
		state.mu.Lock()
		valid := state.enabled && state.inflightToken == item.token && state.version == item.baseVersion
		state.mu.Unlock()
		if !valid {
			for _, rollback := range ordered {
				rollback.finished.CompareAndSwap(preparedSubjectSyncReserved, preparedSubjectSyncOpen)
			}
			return nil, ErrSubjectSyncStalePrepare
		}
	}
	return &PreparedSubjectSyncBatch{items: ordered}, nil
}

// Commit 原子地推进全部预留项的内容版本。
func (b *PreparedSubjectSyncBatch) Commit() error {
	if b == nil || len(b.items) == 0 {
		return ErrSubjectSyncBatchInvalid
	}
	if !b.finished.CompareAndSwap(0, 1) {
		return ErrSubjectSyncFinished
	}
	for _, item := range b.items {
		item.state.mu.Lock()
	}
	defer func() {
		for i := len(b.items) - 1; i >= 0; i-- {
			b.items[i].state.mu.Unlock()
		}
	}()
	for _, item := range b.items {
		state := item.state
		if item.finished.Load() != preparedSubjectSyncReserved || state.inflightToken != item.token || state.version != item.baseVersion {
			b.abortLocked(ErrSubjectSyncStalePrepare)
			return ErrSubjectSyncStalePrepare
		}
	}
	for _, item := range b.items {
		state := item.state
		state.version = item.version
		if state.dirtyGeneration == item.generation {
			state.dirtyMask = 0
			state.fullDirty = false
			state.fullReason = SyncFullReasonNone
		}
		state.inflightToken = 0
		state.inflightSince = time.Time{}
		if state.inflightRelease != nil {
			state.inflightRelease()
			state.inflightRelease = nil
		}
		state.lastError = nil
		item.finished.Store(preparedSubjectSyncFinished)
	}
	return nil
}

func (b *PreparedSubjectSyncBatch) Abort() error { return b.AbortWithError(nil) }

// AbortWithError releases every reservation without consuming dirty state.
func (b *PreparedSubjectSyncBatch) AbortWithError(cause error) error {
	if b == nil || len(b.items) == 0 {
		return ErrSubjectSyncBatchInvalid
	}
	if !b.finished.CompareAndSwap(0, 1) {
		return ErrSubjectSyncFinished
	}
	for _, item := range b.items {
		item.state.mu.Lock()
	}
	defer func() {
		for i := len(b.items) - 1; i >= 0; i-- {
			b.items[i].state.mu.Unlock()
		}
	}()
	b.abortLocked(cause)
	return nil
}

func (b *PreparedSubjectSyncBatch) abortLocked(cause error) {
	for _, item := range b.items {
		state := item.state
		if state.inflightToken == item.token {
			state.inflightToken = 0
			state.inflightSince = time.Time{}
			if state.inflightRelease != nil {
				state.inflightRelease()
				state.inflightRelease = nil
			}
			state.lastError = cause
		}
		item.finished.Store(preparedSubjectSyncFinished)
	}
}
