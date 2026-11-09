package terrain

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// 块副本（可编辑地形方案 P3，D14）：块只归包含它的格的进程写（拥有者），别的进程要看它——邻格要 Ghost 带里的块做
// 移动 / 交接 / 视野判断，每个进程要把玩家附近的块同步给自己的客户端——就在本进程放一份只读副本，由拥有者推送。
//
// 协议（与传输无关：Send 是“非阻塞地发一段字节给某个 sid”，cellworld 的扩展消息就是这样的传输；至多一次，可能丢失、
// 重复、乱序）：
//
//   - 订阅（副本 → 拥有者）：副本按拥有者分组，把想要的块列成一份订阅，附上本地持有的版本，每 Renew 重发一次（租约
//     续期）。拥有者记下订阅（租约 Lease，过期即删），并答复“版本与副本持有的不同”的块：覆盖块（Version ≥ 1）发整块
//     数据，原始块（Version 0）发“原始块”。所以重发的订阅同时是补齐：推送丢了、副本重启了、拥有者重启了（订阅表丢了），
//     至多一个 Renew 之后都对齐。
//   - 推送（拥有者 → 副本）：块提交之后（Changed，可在 Nest 的提交后回调里调），把这块的当前状态发给订阅了它的副本。
//     同一块在发出之前又提交了几次只发最新的一份。
//   - 顺序：副本只认“比本地新”的版本（ReplicaStore.Apply 对旧版本答 ErrStaleOverlay），重复、迟到的消息不回退；
//     每条状态都是整块，不依赖相邻版本，所以丢中间的版本也没关系。
//   - 代际：每条消息带发件进程的原始地形包版本 BaseRevision，与本进程包头不同的整条丢弃（计 BaseRevisionDrops）：
//     拥有者不答别的包版本的订阅，副本不收别的包版本的块。版本号只在同一个 BaseRevision 之内比较——拥有者只在原始
//     地形换版时把旧记录重置为 Version 0（D13），所以换版之前的推送带旧 BaseRevision、到了换版之后的进程一律被丢弃，
//     不会把丢弃掉的覆盖块装回来；换版之后拥有者的版本从 0 起单调递增，比副本持有的覆盖块旧的“原始块”同样按旧版本
//     忽略。同一 BaseRevision 之内 Version 单调且一个版本只对应一份内容，前提是拥有者只推送持久的版本：块的改块事务
//     是 strict 持久（WAL 落盘之后才执行提交后回调），Changed 在提交后回调里调用。
//   - 只认拥有者：一块的状态只接受 OwnerOf(块) 发来的（计 WrongOwner）；拥有者只答自己 Owns 的块。本进程自己写的块
//     不订阅（视图里就是权威数据）。
//
// 限制：拥有者的数据倒退（例如从备份恢复数据库）不在保证内——副本持有的更新版本不会被拉回（计 Ahead），要重启副本
// 进程。各进程的原始地形包版本不同（换版只换了一部分进程）时，两边之间不复制（计 BaseRevisionDrops），副本停在换包前
// 的数据；换版要整组进程换包重启。
//
// 并发：Replicator 的状态（订阅表、想要的块、每个拥有者上次订阅的时间）只在 Run 的 goroutine 上读写；Want / Changed /
// Deliver 只把输入交给它（短锁 + 非阻塞信号），任何 goroutine 都能调。ReplicaStore 与 Source 的读取都在 Run 的 goroutine
// 上：解压、读包（LoadBase）这类活在这里做，不在传输回调或 Nest worker 上。

// ReplicaChannel 区分同一进程里的几份副本：各自订阅、各自存放（例如分区场景里格进程的 Ghost 带与给本进程客户端看的
// 那一份）。线上 1 字节；拥有者按（sid, channel）分别记订阅。
type ReplicaChannel uint8

// ReplicaStore 是一份副本的存放处。Replicator 只在自己的 goroutine 上调用它，同一个 store 不会被并发调用。
type ReplicaStore interface {
	// Held 返回本地持有的版本：0 是原始块（也是没有任何数据时的缺省：客户端与服务端都按原始地形包显示），≥ 1 是覆盖块
	// 的 Version，-1 表示这块现在显示不了（没装入、记录装不进来），要拥有者把当前状态（含“原始块”）发来。
	Held(key ChunkKey) int64
	// Apply 应用拥有者的当前状态：o.Version ≥ 1 是覆盖块（整块数据）；o.Version == 0 表示拥有者那里是原始块（数据为空，
	// 本地持有更新的覆盖块时应忽略并返回 ErrStaleOverlay）。比本地旧的版本返回 ErrStaleOverlay。
	Apply(key ChunkKey, o Overlay) error
	// Forget 表示这一块不再被订阅（Want 去掉了它）：store 可以丢掉它的数据。
	Forget(key ChunkKey)
}

// ReplicatorConfig 配置一个进程的 Replicator。
type ReplicatorConfig struct {
	// Self 是本进程的 sid。Meta 是本进程打开的原始地形包的包头（BaseRevision 与地图范围）。
	Self int32
	Meta Meta
	// Send 非阻塞地把 body 发给 sid（cellworld 的 Node.SendExt）；返回 false 表示丢了（计 SendDrops）。body 交出后
	// Replicator 不再修改。
	Send func(sid int32, body []byte) bool
	// OwnerOf 给出写这块的进程 sid（分区网格：块所在的格的 sid）；false 表示没有拥有者（不订阅）。
	OwnerOf func(ChunkKey) (int32, bool)
	// Source 与 Owns 是拥有者一侧：Source 是本进程的视图（提交后的数据），Owns 报告块是否归本进程写。本进程不写任何块
	// 时 Source 为 nil。
	Source *View
	Owns   func(ChunkKey) bool
	// Renew 是订阅的重发间隔（也是补齐的上限）；零值 1s。Lease 是拥有者保留一份订阅的时间，必须长于 Renew；零值
	// 3 × Renew。Tick 是 Run 检查重发与过期的粒度；零值 Renew / 10（至少 1ms）。
	Renew, Lease, Tick time.Duration
	// Inbox 是收件队列容量（条）；满了丢弃并计数（InboxDrops，订阅重发补齐）。零值 4096。
	Inbox int
	// MaxMessage 是一条块消息的大致字节上限（超过就分成几条）；零值 64KB。
	MaxMessage int
	// Now 是时钟；零值 time.Now。
	Now func() time.Time
	// Applied 可选：一块的新状态应用到 store 之后调用（在 Run 的 goroutine 上，不得阻塞），latency 是拥有者发出这条
	// 消息到应用完的时间（两边的系统时钟之差，只用于测量）。
	Applied func(ch ReplicaChannel, key ChunkKey, version int64, latency time.Duration)
}

// ReplicaStats 是 Replicator 的累计计数快照。
type ReplicaStats struct {
	// SubscribesOut / SubscribesIn：发出 / 收到的订阅（含续期）。Subscribers 是拥有者一侧当前的订阅数。
	SubscribesOut, SubscribesIn, Subscribers uint64
	// ChunksOut：拥有者发出的块状态条数（推送与补齐），其中 PushedOut 是推送。ChunkBytesOut 是块消息的字节。
	ChunksOut, PushedOut, ChunkBytesOut uint64
	// ChunksIn：副本收到的块状态条数；Applied 是应用了的，Stale 是比本地旧（或相同）而忽略的。
	ChunksIn, Applied, Stale uint64
	// BaseRevisionDrops：对方的原始地形包版本与本进程的不同而丢弃（订阅计 1，块状态按条数）。WrongOwner：发件的不是这块的拥有者。Unwanted：
	// 本进程这一路没有订阅这块（订阅刚变）。NotOwned：订阅里有不归本进程写的块（不答）。Unloaded：拥有者自己这块没装入
	// （记录装不进来，不答）。Ahead：副本持有的版本比拥有者的新（拥有者数据倒退，见包说明的限制）。
	BaseRevisionDrops, WrongOwner, Unwanted, NotOwned, Unloaded, Ahead uint64
	// DecodeErrors：坏消息、坏块数据。ApplyErrors：store 拒绝（ErrStaleOverlay 之外）。InboxDrops / SendDrops：收件队列满、
	// 发送被拒。
	DecodeErrors, ApplyErrors, InboxDrops, SendDrops uint64
}

type replicaCounters struct {
	subscribesOut, subscribesIn, subscribers               atomic.Uint64
	chunksOut, pushedOut, chunkBytesOut                    atomic.Uint64
	chunksIn, applied, stale                               atomic.Uint64
	revDrops, wrongOwner, unwanted, notOwned, unloaded     atomic.Uint64
	ahead, decodeErrors, applyErrors, inboxDrops, sendDrop atomic.Uint64
}

// ErrReplicatorConfig 表示 ReplicatorConfig 不合法。
var ErrReplicatorConfig = errors.New("terrain: invalid replicator config")

// Replicator 是一个进程的块副本端点：拥有者一侧（Source 非空）答订阅、推送提交；副本一侧（AddChannel 登记的每一路）
// 订阅、应用。一个进程一个，Run 一次。
type Replicator struct {
	cfg ReplicatorConfig

	// 交给 Run 的输入（任何 goroutine）。
	inbox    chan replicaDelivery
	signal   chan struct{}
	inputMu  sync.Mutex
	changed  map[ChunkKey]struct{}
	wants    map[ReplicaChannel][]ChunkKey
	running  atomic.Bool
	channels map[ReplicaChannel]*replicaChannel // AddChannel 在 Run 之前写，之后只读

	// 只在 Run 的 goroutine 上。
	subs  map[replicaSubKey]*replicaSub
	cache map[ChunkKey]encodedChunk

	stats replicaCounters
}

type replicaDelivery struct {
	from int32
	body []byte
}

type replicaSubKey struct {
	sid int32
	ch  ReplicaChannel
}

// replicaSub 是拥有者记下的一份订阅。
type replicaSub struct {
	keys  map[ChunkKey]struct{}
	until time.Time
}

// replicaChannel 是副本一侧的一路。
type replicaChannel struct {
	store  ReplicaStore
	wanted map[ChunkKey]struct{}
	// byOwner 是想要的块按拥有者分组（键按 ID 升序）；sent 是每个拥有者上次发订阅的时间（零值 = 要尽快发）。
	byOwner map[int32][]ChunkKey
	sent    map[int32]time.Time
}

// encodedChunk 是拥有者编码过的一块（按版本缓存：推送给几个订阅者、补齐时复用）。
type encodedChunk struct {
	version int64
	data    []byte
}

// replicaCacheLimit 是拥有者编码缓存的块数上限（满了清空重来；每块约 3KB）。
const replicaCacheLimit = 4096

// NewReplicator 校验配置并构造 Replicator。
func NewReplicator(cfg ReplicatorConfig) (*Replicator, error) {
	if cfg.Renew == 0 {
		cfg.Renew = time.Second
	}
	if cfg.Lease == 0 {
		cfg.Lease = 3 * cfg.Renew
	}
	if cfg.Tick == 0 {
		cfg.Tick = max(cfg.Renew/10, time.Millisecond)
	}
	if cfg.Inbox == 0 {
		cfg.Inbox = 4096
	}
	if cfg.MaxMessage == 0 {
		cfg.MaxMessage = 64 << 10
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	switch {
	case cfg.Self == 0 || cfg.Send == nil || cfg.OwnerOf == nil:
		return nil, fmt.Errorf("%w: self, send and owner lookup are required", ErrReplicatorConfig)
	case cfg.Source != nil && cfg.Owns == nil:
		return nil, fmt.Errorf("%w: an owner needs Owns", ErrReplicatorConfig)
	case cfg.Source != nil && cfg.Source.Meta() != cfg.Meta:
		return nil, fmt.Errorf("%w: the source view is on another pack", ErrReplicatorConfig)
	case cfg.Renew <= 0 || cfg.Lease <= cfg.Renew || cfg.Tick <= 0 || cfg.Inbox < 0 || cfg.MaxMessage <= 0:
		return nil, fmt.Errorf("%w: renew %v must be positive and lease %v longer", ErrReplicatorConfig, cfg.Renew, cfg.Lease)
	}
	return &Replicator{
		cfg: cfg, inbox: make(chan replicaDelivery, cfg.Inbox), signal: make(chan struct{}, 1),
		changed: map[ChunkKey]struct{}{}, wants: map[ReplicaChannel][]ChunkKey{}, channels: map[ReplicaChannel]*replicaChannel{},
		subs: map[replicaSubKey]*replicaSub{}, cache: map[ChunkKey]encodedChunk{},
	}, nil
}

// AddChannel 登记副本的一路（Run 之前）。
func (r *Replicator) AddChannel(ch ReplicaChannel, store ReplicaStore) error {
	if r.running.Load() {
		return fmt.Errorf("%w: channel %d added after Run", ErrReplicatorConfig, ch)
	}
	if store == nil || r.channels[ch] != nil {
		return fmt.Errorf("%w: channel %d needs one store", ErrReplicatorConfig, ch)
	}
	r.channels[ch] = &replicaChannel{store: store, wanted: map[ChunkKey]struct{}{}, byOwner: map[int32][]ChunkKey{}, sent: map[int32]time.Time{}}
	return nil
}

// Want 换掉一路想要的块（整份替换）。新加的块尽快订阅，去掉的块交给 store.Forget；任何 goroutine 可调，不阻塞。
func (r *Replicator) Want(ch ReplicaChannel, keys []ChunkKey) {
	keys = slices.Clone(keys)
	r.inputMu.Lock()
	r.wants[ch] = keys
	r.inputMu.Unlock()
	r.wake()
}

// Changed 报告本进程写的一块刚提交（新版本已在 Source 里）：尽快推给订阅了它的副本。只登记、不阻塞，可以在 Nest 的
// 提交后回调里调用。
func (r *Replicator) Changed(key ChunkKey) {
	r.inputMu.Lock()
	r.changed[key] = struct{}{}
	r.inputMu.Unlock()
	r.wake()
}

// Deliver 交给 Replicator 一条收到的消息（传输回调，例如 cellworld 的扩展消息处理器）：复制 body、非阻塞入队，队列满时
// 丢弃并返回 false。
func (r *Replicator) Deliver(from int32, body []byte) bool {
	select {
	case r.inbox <- replicaDelivery{from: from, body: append([]byte(nil), body...)}:
		return true
	default:
		r.stats.inboxDrops.Add(1)
		return false
	}
}

func (r *Replicator) wake() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Stats 返回计数快照，任何 goroutine 可调。
func (r *Replicator) Stats() ReplicaStats {
	s := &r.stats
	return ReplicaStats{
		SubscribesOut: s.subscribesOut.Load(), SubscribesIn: s.subscribesIn.Load(), Subscribers: s.subscribers.Load(),
		ChunksOut: s.chunksOut.Load(), PushedOut: s.pushedOut.Load(), ChunkBytesOut: s.chunkBytesOut.Load(),
		ChunksIn: s.chunksIn.Load(), Applied: s.applied.Load(), Stale: s.stale.Load(),
		BaseRevisionDrops: s.revDrops.Load(), WrongOwner: s.wrongOwner.Load(), Unwanted: s.unwanted.Load(),
		NotOwned: s.notOwned.Load(), Unloaded: s.unloaded.Load(), Ahead: s.ahead.Load(),
		DecodeErrors: s.decodeErrors.Load(), ApplyErrors: s.applyErrors.Load(), InboxDrops: s.inboxDrops.Load(), SendDrops: s.sendDrop.Load(),
	}
}

// Run 处理收件、推送、订阅续期与订阅过期，直到 ctx 结束。只能 Run 一次。
func (r *Replicator) Run(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return errors.New("terrain: replicator already ran")
	}
	ticker := time.NewTicker(r.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case d := <-r.inbox:
			r.handle(d)
		case <-r.signal:
			r.takeInput()
		case <-ticker.C:
			r.tick()
		}
	}
}

// takeInput 处理 Want 与 Changed 交来的输入。
func (r *Replicator) takeInput() {
	r.inputMu.Lock()
	wants, changed := r.wants, r.changed
	r.wants, r.changed = map[ReplicaChannel][]ChunkKey{}, map[ChunkKey]struct{}{}
	r.inputMu.Unlock()
	for ch, keys := range wants {
		if c := r.channels[ch]; c != nil {
			r.setWanted(c, keys)
		}
	}
	if len(wants) > 0 {
		r.renew(true)
	}
	if len(changed) > 0 {
		r.push(changed)
	}
}

// tick 是定时的部分：到期的订阅续期、过期的订阅删除。
func (r *Replicator) tick() {
	r.renew(false)
	now := r.cfg.Now()
	for key, sub := range r.subs {
		if now.After(sub.until) {
			delete(r.subs, key)
		}
	}
	r.stats.subscribers.Store(uint64(len(r.subs)))
}

// ---- 副本一侧 ----

// setWanted 换掉一路想要的块：按拥有者重新分组；分组变了的拥有者尽快重订（sent 清零），不再想要任何块的拥有者发一份
// 空订阅（撤销），去掉的块交给 store.Forget。
func (r *Replicator) setWanted(c *replicaChannel, keys []ChunkKey) {
	next := make(map[ChunkKey]struct{}, len(keys))
	byOwner := map[int32][]ChunkKey{}
	for _, key := range keys {
		if _, dup := next[key]; dup || !r.cfg.Meta.Contains(key) {
			continue
		}
		owner, ok := r.cfg.OwnerOf(key)
		if !ok || owner == 0 || owner == r.cfg.Self {
			continue // 没有拥有者，或者是本进程自己写的块（视图里就是权威数据）
		}
		next[key] = struct{}{}
		byOwner[owner] = append(byOwner[owner], key)
	}
	for owner, list := range byOwner {
		slices.SortFunc(list, func(a, b ChunkKey) int { return cmp.Compare(a.ID(), b.ID()) })
		if !slices.Equal(list, c.byOwner[owner]) {
			c.sent[owner] = time.Time{}
		}
	}
	for owner := range c.byOwner {
		if _, still := byOwner[owner]; !still {
			c.sent[owner] = time.Time{} // 撤销：下一次 renew 发空订阅
		}
	}
	for key := range c.wanted {
		if _, still := next[key]; !still {
			c.store.Forget(key)
		}
	}
	c.wanted, c.byOwner = next, byOwner
}

// renew 发到期的订阅：分组刚变的拥有者（sent 为零值或还没有）立即发；onlyChanged 为假时，距上次发满 Renew 的也发
// （续期兼补齐）。不再想要任何块的拥有者发一份空订阅撤销（只发一次：丢了由拥有者那边的租约过期兜底）。
func (r *Replicator) renew(onlyChanged bool) {
	now := r.cfg.Now()
	for ch, c := range r.channels {
		for owner, keys := range c.byOwner {
			last, known := c.sent[owner]
			if !known || last.IsZero() || (!onlyChanged && now.Sub(last) >= r.cfg.Renew) {
				r.subscribe(ch, c, owner, keys)
				c.sent[owner] = now
			}
		}
		for owner, last := range c.sent {
			if _, wanted := c.byOwner[owner]; wanted {
				continue
			}
			if last.IsZero() {
				r.subscribe(ch, c, owner, nil)
			}
			delete(c.sent, owner)
		}
	}
}

func (r *Replicator) subscribe(ch ReplicaChannel, c *replicaChannel, owner int32, keys []ChunkKey) {
	held := make([]int64, len(keys))
	for i, key := range keys {
		held[i] = c.store.Held(key)
	}
	r.send(owner, encodeSubscribe(ch, r.cfg.Meta.BaseRevision, keys, held))
	r.stats.subscribesOut.Add(1)
}

// applyChunks 应用拥有者发来的块状态。
func (r *Replicator) applyChunks(from int32, m chunksMsg) {
	c := r.channels[m.channel]
	if m.rev != r.cfg.Meta.BaseRevision {
		r.stats.chunksIn.Add(uint64(len(m.entries)))
		r.stats.revDrops.Add(uint64(len(m.entries)))
		return
	}
	now := r.cfg.Now()
	for _, e := range m.entries {
		r.stats.chunksIn.Add(1)
		switch owner, ok := r.cfg.OwnerOf(e.key); {
		case c == nil:
			r.stats.unwanted.Add(1)
			continue
		case !ok || owner != from:
			r.stats.wrongOwner.Add(1)
			continue
		}
		if _, wanted := c.wanted[e.key]; !wanted {
			r.stats.unwanted.Add(1)
			continue
		}
		o := Overlay{BaseRevision: m.rev, Version: e.version}
		if e.version >= 1 {
			chunk, err := decodeChunk(e.data)
			if err != nil {
				r.stats.decodeErrors.Add(1)
				continue
			}
			o.Heights, o.Materials = chunk.Heights, chunk.Materials
		}
		switch err := c.store.Apply(e.key, o); {
		case err == nil:
			r.stats.applied.Add(1)
			if hook := r.cfg.Applied; hook != nil {
				hook(m.channel, e.key, e.version, now.Sub(time.Unix(0, m.sentAt)))
			}
		case errors.Is(err, ErrStaleOverlay):
			r.stats.stale.Add(1)
		default:
			r.stats.applyErrors.Add(1)
		}
	}
}

// ---- 拥有者一侧 ----

// answer 处理一份订阅：记下（空订阅是撤销），答复版本与副本持有的不同的块。
func (r *Replicator) answer(from int32, m subscribeMsg) {
	r.stats.subscribesIn.Add(1)
	if m.rev != r.cfg.Meta.BaseRevision {
		r.stats.revDrops.Add(1) // 对方在另一版原始地形上：这边的任何状态对它都没用，不记订阅、不答
		return
	}
	if r.cfg.Source == nil {
		r.stats.notOwned.Add(uint64(len(m.keys)))
		return
	}
	subKey := replicaSubKey{sid: from, ch: m.channel}
	if len(m.keys) == 0 {
		delete(r.subs, subKey)
		r.stats.subscribers.Store(uint64(len(r.subs)))
		return
	}
	keys := make(map[ChunkKey]struct{}, len(m.keys))
	var batch chunkBatch
	for i, key := range m.keys {
		if !r.cfg.Owns(key) {
			r.stats.notOwned.Add(1)
			continue
		}
		keys[key] = struct{}{}
		version, loaded := r.cfg.Source.Version(key)
		switch held := m.held[i]; {
		case !loaded:
			r.stats.unloaded.Add(1)
		case held > version:
			r.stats.ahead.Add(1)
		case held != version:
			if e, ok := r.entry(key, version); ok {
				batch.add(r, from, m.channel, e, false)
			}
		}
	}
	batch.flush(r)
	r.subs[subKey] = &replicaSub{keys: keys, until: r.cfg.Now().Add(r.cfg.Lease)}
	r.stats.subscribers.Store(uint64(len(r.subs)))
}

// push 把刚提交的块推给订阅了它的副本（每块取 Source 里的当前状态：同一块提交了几次只发最新的）。
func (r *Replicator) push(changed map[ChunkKey]struct{}) {
	if r.cfg.Source == nil || len(r.subs) == 0 {
		return
	}
	now := r.cfg.Now()
	keys := make([]ChunkKey, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b ChunkKey) int { return cmp.Compare(a.ID(), b.ID()) })
	subKeys := make([]replicaSubKey, 0, len(r.subs))
	for sk := range r.subs {
		subKeys = append(subKeys, sk)
	}
	slices.SortFunc(subKeys, func(a, b replicaSubKey) int { return cmp.Or(cmp.Compare(a.sid, b.sid), cmp.Compare(a.ch, b.ch)) })
	for _, sk := range subKeys {
		sub := r.subs[sk]
		if now.After(sub.until) {
			continue
		}
		var batch chunkBatch
		for _, key := range keys {
			if _, subscribed := sub.keys[key]; !subscribed || !r.cfg.Owns(key) {
				continue
			}
			version, loaded := r.cfg.Source.Version(key)
			if !loaded {
				r.stats.unloaded.Add(1)
				continue
			}
			if e, ok := r.entry(key, version); ok {
				batch.add(r, sk.sid, sk.ch, e, true)
			}
		}
		batch.flush(r)
	}
}

// entry 是一块的当前状态（覆盖块编码整块数据，按版本缓存）。读视图与编码失败时返回 false（记 Unloaded）。
func (r *Replicator) entry(key ChunkKey, version int64) (chunkEntry, bool) {
	e := chunkEntry{key: key, version: version}
	if version == 0 {
		return e, true
	}
	if cached, ok := r.cache[key]; ok && cached.version == version {
		e.data = cached.data
		return e, true
	}
	chunk, got, loaded := r.cfg.Source.Chunk(key)
	if !loaded || got == 0 {
		r.stats.unloaded.Add(1)
		return e, false
	}
	data, err := encodeChunk(chunk)
	if err != nil {
		r.stats.decodeErrors.Add(1)
		return e, false
	}
	if len(r.cache) >= replicaCacheLimit {
		clear(r.cache)
	}
	e.version, e.data = got, data
	r.cache[key] = encodedChunk{version: got, data: data}
	return e, true
}

// chunkBatch 把发给同一个（sid, channel）的块状态攒成不超过 MaxMessage 的消息。
type chunkBatch struct {
	sid     int32
	ch      ReplicaChannel
	entries []chunkEntry
	size    int
	pushed  int
}

func (b *chunkBatch) add(r *Replicator, sid int32, ch ReplicaChannel, e chunkEntry, pushed bool) {
	b.sid, b.ch = sid, ch
	if len(b.entries) > 0 && b.size+len(e.data) > r.cfg.MaxMessage {
		b.flush(r)
	}
	b.entries = append(b.entries, e)
	b.size += len(e.data) + 24
	if pushed {
		b.pushed++
	}
}

func (b *chunkBatch) flush(r *Replicator) {
	if len(b.entries) == 0 {
		return
	}
	body := encodeChunks(b.ch, r.cfg.Meta.BaseRevision, r.cfg.Now().UnixNano(), b.entries)
	if r.send(b.sid, body) {
		r.stats.chunksOut.Add(uint64(len(b.entries)))
		r.stats.pushedOut.Add(uint64(b.pushed))
		r.stats.chunkBytesOut.Add(uint64(len(body)))
	}
	b.entries, b.size, b.pushed = b.entries[:0], 0, 0
}

func (r *Replicator) send(sid int32, body []byte) bool {
	if !r.cfg.Send(sid, body) {
		r.stats.sendDrop.Add(1)
		return false
	}
	return true
}

// handle 解码一条收件并交给对应的一侧。
func (r *Replicator) handle(d replicaDelivery) {
	m, err := decodeReplicaMsg(d.body)
	if err != nil {
		r.stats.decodeErrors.Add(1)
		return
	}
	switch m := m.(type) {
	case subscribeMsg:
		r.answer(d.from, m)
	case chunksMsg:
		r.applyChunks(d.from, m)
	}
}

// ---- 线上格式 ----
//
// 每条消息：版本 1 字节（replicaWireVersion）、类型 1 字节、channel 1 字节、varint 发件进程的原始地形包版本
// BaseRevision，然后：
//
//	订阅（replicaSubscribe）：uvarint 块数 n；n 个块 ID（ChunkKey.ID，升序，与前一个的差 uvarint）；n 个持有版本（zigzag
//	                         varint：-1 显示不了、0 原始块、≥ 1 覆盖块）。
//	块状态（replicaChunks）：varint 发出时间（UnixNano，只用于测量）；uvarint 条数；每条 uvarint 块 ID、uvarint Version，
//	                         Version ≥ 1 时再跟 uvarint 长度与整块数据（原始地形包的块编码：平面预测 + DEFLATE，见 codec.go）。
//
// 版本不同的消息按坏消息丢弃（未上线，不做跨版本兼容）。

const (
	replicaWireVersion = 1
	replicaSubscribe   = 1
	replicaChunks      = 2
	// replicaMaxKeys / replicaMaxData 是解码的上限：一份订阅至多这么多块（一个格 48 × 48 块的 10 倍余量），一块数据至多
	// 这么多字节（明文上限 16KB，DEFLATE 不会把它放大到 64KB）。
	replicaMaxKeys = 1 << 15
	replicaMaxData = 64 << 10
)

var errReplicaMessage = errors.New("terrain: malformed replica message")

type subscribeMsg struct {
	channel ReplicaChannel
	rev     int64
	keys    []ChunkKey
	held    []int64
}

type chunksMsg struct {
	channel ReplicaChannel
	rev     int64
	sentAt  int64
	entries []chunkEntry
}

type chunkEntry struct {
	key     ChunkKey
	version int64
	data    []byte
}

func appendReplicaHeader(b []byte, kind byte, ch ReplicaChannel, rev int64) []byte {
	b = append(b, replicaWireVersion, kind, byte(ch))
	return binary.AppendVarint(b, rev)
}

// encodeSubscribe 编码订阅；keys 已按 ID 升序，held 与它一一对应。
func encodeSubscribe(ch ReplicaChannel, rev int64, keys []ChunkKey, held []int64) []byte {
	b := make([]byte, 0, 3+2*binary.MaxVarintLen64+len(keys)*4)
	b = appendReplicaHeader(b, replicaSubscribe, ch, rev)
	b = binary.AppendUvarint(b, uint64(len(keys)))
	var prev uint64
	for _, key := range keys {
		id := key.ID()
		b = binary.AppendUvarint(b, id-prev)
		prev = id
	}
	for _, h := range held {
		b = binary.AppendVarint(b, h)
	}
	return b
}

func encodeChunks(ch ReplicaChannel, rev, sentAt int64, entries []chunkEntry) []byte {
	size := 3 + 3*binary.MaxVarintLen64
	for _, e := range entries {
		size += 3*binary.MaxVarintLen64 + len(e.data)
	}
	b := make([]byte, 0, size)
	b = appendReplicaHeader(b, replicaChunks, ch, rev)
	b = binary.AppendVarint(b, sentAt)
	b = binary.AppendUvarint(b, uint64(len(entries)))
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.key.ID())
		b = binary.AppendUvarint(b, uint64(e.version))
		if e.version >= 1 {
			b = binary.AppendUvarint(b, uint64(len(e.data)))
			b = append(b, e.data...)
		}
	}
	return b
}

// replicaReader 顺序读消息体；第一次出错之后的读取都返回零值，err 记下第一个错误。
type replicaReader struct {
	b   []byte
	err error
}

func (rd *replicaReader) fail(what string) {
	if rd.err == nil {
		rd.err = fmt.Errorf("%w: %s", errReplicaMessage, what)
	}
}

func (rd *replicaReader) uvarint(what string) uint64 {
	if rd.err != nil {
		return 0
	}
	v, n := binary.Uvarint(rd.b)
	if n <= 0 {
		rd.fail(what)
		return 0
	}
	rd.b = rd.b[n:]
	return v
}

func (rd *replicaReader) varint(what string) int64 {
	if rd.err != nil {
		return 0
	}
	v, n := binary.Varint(rd.b)
	if n <= 0 {
		rd.fail(what)
		return 0
	}
	rd.b = rd.b[n:]
	return v
}

func (rd *replicaReader) bytes(n uint64, what string) []byte {
	if rd.err != nil {
		return nil
	}
	if n > uint64(len(rd.b)) {
		rd.fail(what)
		return nil
	}
	out := rd.b[:n:n]
	rd.b = rd.b[n:]
	return out
}

// decodeReplicaMsg 解码一条消息（subscribeMsg 或 chunksMsg）；多余的尾部字节、越界的计数、版本不同都算坏消息。
func decodeReplicaMsg(body []byte) (any, error) {
	if len(body) < 3 {
		return nil, fmt.Errorf("%w: %d bytes", errReplicaMessage, len(body))
	}
	if body[0] != replicaWireVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", errReplicaMessage, body[0], replicaWireVersion)
	}
	kind, ch := body[1], ReplicaChannel(body[2])
	rd := &replicaReader{b: body[3:]}
	rev := rd.varint("base revision")
	var out any
	switch kind {
	case replicaSubscribe:
		n := rd.uvarint("key count")
		if n > replicaMaxKeys {
			return nil, fmt.Errorf("%w: %d keys", errReplicaMessage, n)
		}
		m := subscribeMsg{channel: ch, rev: rev, keys: make([]ChunkKey, 0, n), held: make([]int64, 0, n)}
		var id uint64
		for i := range n {
			delta := rd.uvarint("key")
			if i > 0 && delta == 0 {
				rd.fail("keys not ascending")
			}
			id += delta
			m.keys = append(m.keys, KeyFromID(id))
		}
		for range n {
			m.held = append(m.held, rd.varint("held version"))
		}
		out = m
	case replicaChunks:
		m := chunksMsg{channel: ch, rev: rev, sentAt: rd.varint("sent at")}
		n := rd.uvarint("entry count")
		if n > replicaMaxKeys {
			return nil, fmt.Errorf("%w: %d entries", errReplicaMessage, n)
		}
		for range n {
			e := chunkEntry{key: KeyFromID(rd.uvarint("key"))}
			version := rd.uvarint("version")
			if version > 1<<62 {
				rd.fail("version")
			}
			e.version = int64(version)
			if e.version >= 1 {
				size := rd.uvarint("data length")
				if size > replicaMaxData {
					rd.fail("data length")
				}
				e.data = rd.bytes(size, "data")
			}
			m.entries = append(m.entries, e)
		}
		out = m
	default:
		return nil, fmt.Errorf("%w: type %d", errReplicaMessage, kind)
	}
	if rd.err != nil {
		return nil, rd.err
	}
	if len(rd.b) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", errReplicaMessage, len(rd.b))
	}
	return out, nil
}
