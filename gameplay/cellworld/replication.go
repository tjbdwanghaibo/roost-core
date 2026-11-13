package cellworld

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 内部复制与客户端 EntitySync 分离。队列只合并完整状态；发送时比较上次发出的
// 基线，接收方必须精确匹配 base version，绝不猜差值。缺口只拒绝解不出的部分：增量对不上的对象从接收缓存删掉，
// 引用它的视野这一拍不交付；成员表对不上的观察者不交付、不推进。接收方只请求发送方忘掉这些对象与观察者的基线
// （replicaReset），不让一次丢包把整个 peer 退回全量。缓存可丢弃、总量有界，不是业务权威。周期全量兜住恢复请求
// 也丢失的情况。
type stateDelta struct {
	ID    int64        `msgpack:"i"`
	Ver   Version      `msgpack:"v"`
	Base  Version      `msgpack:"b,omitempty"`
	Full  *EntityState `msgpack:"f,omitempty"`
	Mask  uint8        `msgpack:"m,omitempty"`
	X     int64        `msgpack:"x,omitempty"`
	Y     int64        `msgpack:"y,omitempty"`
	VX    int64        `msgpack:"u,omitempty"`
	VY    int64        `msgpack:"w,omitempty"`
	HP    int32        `msgpack:"h,omitempty"`
	Idle  bool         `msgpack:"l,omitempty"`
	Panel Panel        `msgpack:"p,omitempty"`
}

const (
	deltaX uint8 = 1 << iota
	deltaY
	deltaVX
	deltaVY
	deltaHP
	deltaIdle
	deltaPanel
)

func diffState(old, next EntityState) stateDelta {
	d := stateDelta{ID: next.ID, Ver: next.Ver, Base: old.Ver}
	if old.X != next.X {
		d.Mask |= deltaX
		d.X = next.X
	}
	if old.Y != next.Y {
		d.Mask |= deltaY
		d.Y = next.Y
	}
	if old.VX != next.VX {
		d.Mask |= deltaVX
		d.VX = next.VX
	}
	if old.VY != next.VY {
		d.Mask |= deltaVY
		d.VY = next.VY
	}
	if old.HP != next.HP {
		d.Mask |= deltaHP
		d.HP = next.HP
	}
	if old.Idle != next.Idle {
		d.Mask |= deltaIdle
		d.Idle = next.Idle
	}
	if old.Panel != next.Panel {
		d.Mask |= deltaPanel
		d.Panel = next.Panel
	}
	return d
}
func (d stateDelta) apply(base EntityState) (EntityState, bool) {
	if d.ID == 0 {
		return EntityState{}, false
	}
	if d.Full != nil {
		return *d.Full, d.Full.ID == d.ID && d.Full.Ver == d.Ver
	}
	if base.ID != d.ID || base.Ver != d.Base || d.Mask & ^uint8(127) != 0 {
		return EntityState{}, false
	}
	base.Ver = d.Ver
	if d.Mask&deltaX != 0 {
		base.X = d.X
	}
	if d.Mask&deltaY != 0 {
		base.Y = d.Y
	}
	if d.Mask&deltaVX != 0 {
		base.VX = d.VX
	}
	if d.Mask&deltaVY != 0 {
		base.VY = d.VY
	}
	if d.Mask&deltaHP != 0 {
		base.HP = d.HP
	}
	if d.Mask&deltaIdle != 0 {
		base.Idle = d.Idle
	}
	if d.Mask&deltaPanel != 0 {
		base.Panel = d.Panel
	}
	return base, true
}

type replicaKey struct {
	peer int32
	kind msgKind
	id   int64
}
type stateBaseline struct {
	state  EntityState
	fullAt time.Time
}
type viewBaseline struct {
	epoch, tick uint64
	ids         []int64
	fullAt      time.Time
}
type replicaCache struct {
	mu         sync.Mutex
	states     map[replicaKey]stateBaseline
	views      map[replicaKey]viewBaseline
	resetAfter map[resetKey]time.Time // 接收方：每个对象 / 观察者上一次请求重置后的静默期
}

// resetKey 标识接收方请求过重置的一个对象（view=false，kind 是消息种类）或一个观察者的成员表（view=true）。
type resetKey struct {
	peer int32
	view bool
	kind msgKind
	id   int64
}

func (c *replicaCache) init() {
	c.states = make(map[replicaKey]stateBaseline)
	c.views = make(map[replicaKey]viewBaseline)
	c.resetAfter = make(map[resetKey]time.Time)
}

// forget 是发送方收到 replicaReset：只删列出的对象与观察者的基线；nil 时删这个 peer 的全部。
func (c *replicaCache) forget(peer int32, r *replicaReset) {
	if r == nil {
		c.clearPeer(peer)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range r.States {
		delete(c.states, replicaKey{peer, r.Kind, id})
	}
	for _, observer := range r.Views {
		delete(c.views, replicaKey{peer, kindWitness, observer})
	}
}
func (c *replicaCache) clearPeer(peer int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.states {
		if k.peer == peer {
			delete(c.states, k)
		}
	}
	for k := range c.views {
		if k.peer == peer {
			delete(c.views, k)
		}
	}
}
func (c *replicaCache) bound(limit int) {
	if len(c.states)+len(c.views) >= limit {
		clear(c.states)
		clear(c.views)
	}
}

var errReplicaGap = errors.New("cellworld: internal replica baseline gap")

// replicaGap 是一条消息里解不出来的部分（errors.Is(err, errReplicaGap)）；decodeReplica 同时返回解得出的部分。
type replicaGap struct {
	kind   msgKind
	states []int64 // 增量对不上、或视野引用了却没有基线的对象
	views  []int64 // 成员表基线对不上的观察者
}

func (g *replicaGap) Error() string {
	return fmt.Sprintf("%v: %d objects, %d views", errReplicaGap, len(g.states), len(g.views))
}
func (g *replicaGap) Is(target error) bool { return target == errReplicaGap }

// refreshDue 判断一个观察者的成员表是否该发全量：距上次全量满一个 ReplicaRefresh，或越过了它所在区块的刷新相位边界。
// 相位按观察者位置所在的区块（边长 2R）取：同一块的观察者在 witnesses 里排在一起、合进同一批消息，到点一起发全量，
// 被强制全量的成员状态在一条消息里只编一次；不同块的相位散开，不挤在同一拍。越过边界的那次可能早于满一个周期，
// 全量间隔仍不超过 ReplicaRefresh。
func (n *Node) refreshDue(fullAt, now time.Time, at spatial.Point) bool {
	refresh := int64(n.cfg.ReplicaRefresh)
	if !now.Before(fullAt.Add(n.cfg.ReplicaRefresh)) || refresh <= 0 {
		return true
	}
	phase := int64(0)
	if bx, by, ok := n.refreshBlock(at); ok {
		h := uint64(bx)*0x9E3779B97F4A7C15 ^ uint64(by)*0xC2B2AE3D27D4EB4F
		h ^= h >> 29
		h *= 0xBF58476D1CE4E5B9
		h ^= h >> 32
		phase = int64(h % uint64(refresh))
	}
	slot := func(t time.Time) int64 { return floorDiv(t.UnixNano()+phase, refresh) }
	return slot(now) != slot(fullAt)
}

// refreshBlock 返回位置所在的刷新区块（边长 2 × ViewRadius）；ViewRadius 未设时没有区块。
func (n *Node) refreshBlock(at spatial.Point) (bx, by int64, ok bool) {
	size := 2 * n.cfg.ViewRadius
	if size <= 0 {
		return 0, 0, false
	}
	return floorDiv(at.X, size), floorDiv(at.Y, size), true
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// encodeReplica 只由状态发送线程调用；传输返回错误后清除该 peer 的发送基线。
func (n *Node) encodeReplica(sid int32, input *wireMsg, now time.Time) *wireMsg {
	if input.Kind != kindGhost && input.Kind != kindWitness {
		return input
	}
	c := &n.sentReplica
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound(n.cfg.ReplicaCacheSize)
	out := *input
	out.Replica = true
	out.Deltas = nil
	states := map[int64]EntityState{}
	forceFull := map[int64]bool{}
	add := func(s EntityState) {
		if old, ok := states[s.ID]; !ok || s.Ver.Newer(old.Ver) {
			states[s.ID] = s
		}
	}
	if input.Kind == kindGhost {
		ghost := *input.Ghost
		out.Ghost = &ghost
		for _, s := range ghost.Updates {
			add(s)
		}
		ghost.Updates = nil
		for _, r := range ghost.Removes {
			delete(c.states, replicaKey{sid, kindGhost, r.ID})
		}
	} else {
		out.Witness = nil
		out.Witnesses = nil
		views := append([]*witnessMsg{input.Witness}, input.Witnesses...)
		for _, v := range views {
			w := *v
			key := replicaKey{sid, kindWitness, w.Observer}
			old, ok := c.views[key]
			ids := make([]int64, 0, len(v.Entities))
			for _, s := range v.Entities {
				add(s)
				ids = append(ids, s.ID)
			}
			slices.Sort(ids)
			ids = slices.Compact(ids)
			w.Entities = nil
			w.Added = nil
			w.Removed = nil
			full := !ok || old.epoch != w.Epoch || n.refreshDue(old.fullAt, now, w.Self.Pos())
			w.FullMembership = full
			if full {
				for _, id := range ids {
					forceFull[id] = true
				}
				w.Added = ids
				w.BaseTick = 0
				old.fullAt = now
			} else {
				w.BaseTick = old.tick
				w.Added, w.Removed = diffMembership(old.ids, ids)
			}
			c.views[key] = viewBaseline{epoch: w.Epoch, tick: w.Tick, ids: ids, fullAt: old.fullAt}
			if out.Witness == nil {
				out.Witness = &w
			} else {
				out.Witnesses = append(out.Witnesses, &w)
			}
		}
	}
	ids := make([]int64, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s := states[id]
		key := replicaKey{sid, input.Kind, id}
		old, ok := c.states[key]
		// Witness 的新 Tick 自带存活事实；Ghost 不能省略空差值，否则接收方收不到续活。
		if input.Kind == kindWitness && ok && !forceFull[id] && old.state == s && now.Before(old.fullAt.Add(n.cfg.ReplicaRefresh)) {
			continue
		}
		if !ok || forceFull[id] || old.state.Kind != s.Kind || old.state.Life != s.Life || old.state.Base != s.Base || old.state.Ver.Epoch != s.Ver.Epoch || !now.Before(old.fullAt.Add(n.cfg.ReplicaRefresh)) {
			out.Deltas = append(out.Deltas, stateDelta{ID: s.ID, Ver: s.Ver, Full: &s})
			old.fullAt = now
			n.stats.replicaFull.Add(1)
		} else {
			out.Deltas = append(out.Deltas, diffState(old.state, s))
			n.stats.replicaDelta.Add(1)
		}
		c.states[key] = stateBaseline{state: s, fullAt: old.fullAt}
	}
	return &out
}
func diffMembership(old, next []int64) (add, remove []int64) {
	i, j := 0, 0
	for i < len(old) || j < len(next) {
		switch {
		case i == len(old):
			add = append(add, next[j:]...)
			return
		case j == len(next):
			remove = append(remove, old[i:]...)
			return
		case old[i] < next[j]:
			remove = append(remove, old[i])
			i++
		case old[i] > next[j]:
			add = append(add, next[j])
			j++
		default:
			i++
			j++
		}
	}
	return
}

// decodeReplica 在网络回调里只重建值副本，整条消息验证后才推进缓存；真正业务状态仍由各自 Nest handler 校验版本并提交。
// 有缺口时返回解得出的部分与 *replicaGap：增量对不上的对象不进缓存（已有的旧基线也删掉，不能被当成当前内容）；
// 引用了缺失对象的视野不交付，但成员表照常推进（成员表本身解得出，下一份增量成员表仍能接上）；成员表对不上的
// 观察者不交付、不推进。坏消息（格式或数量越界）整条拒绝，不推进任何缓存。
func (n *Node) decodeReplica(input *wireMsg) ([]*wireMsg, error) {
	if !input.Replica {
		return []*wireMsg{input}, nil
	}
	if input.Kind != kindGhost && input.Kind != kindWitness {
		return nil, errBadMessage
	}
	c := &n.receivedReplica
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound(n.cfg.ReplicaCacheSize)
	if len(input.Deltas) > n.cfg.ReplicaCacheSize || len(input.Witnesses) > n.cfg.WitnessBatch {
		return nil, errBadMessage
	}
	gap := &replicaGap{kind: input.Kind}
	reported := map[int64]bool{}
	missing := func(id int64) {
		if !reported[id] {
			reported[id] = true
			gap.states = append(gap.states, id)
		}
	}
	updates := make(map[replicaKey]stateBaseline, len(input.Deltas))
	broken := map[int64]bool{}
	get := func(id int64) (EntityState, bool) {
		if broken[id] {
			return EntityState{}, false
		}
		key := replicaKey{input.From, input.Kind, id}
		if v, ok := updates[key]; ok {
			return v.state, true
		}
		v, ok := c.states[key]
		return v.state, ok
	}
	for _, d := range input.Deltas {
		key := replicaKey{input.From, input.Kind, d.ID}
		old, _ := get(d.ID)
		state, ok := old, true
		// 迟到/重复包不能倒退解码基线；业务侧还会核验对象版本。
		if old.ID == 0 || d.Ver.Newer(old.Ver) {
			state, ok = d.apply(old)
		}
		if !ok || broken[d.ID] {
			broken[d.ID] = true
			delete(updates, key)
			missing(d.ID)
			continue
		}
		updates[key] = stateBaseline{state: state}
	}
	messages := make([]*wireMsg, 0, 1+len(input.Witnesses))
	views := make(map[replicaKey]viewBaseline, 1+len(input.Witnesses))
	if input.Kind == kindGhost {
		m := *input
		b := *input.Ghost
		m.Ghost = &b
		b.Updates = nil
		for _, d := range input.Deltas {
			if state, ok := get(d.ID); ok {
				b.Updates = append(b.Updates, state)
			}
		}
		if len(broken) == 0 || len(b.Updates)+len(b.Removes) > 0 {
			messages = append(messages, &m)
		}
	} else {
		for _, v := range append([]*witnessMsg{input.Witness}, input.Witnesses...) {
			if v == nil {
				return nil, errBadMessage
			}
			key := replicaKey{input.From, kindWitness, v.Observer}
			old, ok := c.views[key]
			if ok && (v.Epoch < old.epoch || v.Epoch == old.epoch && v.Tick <= old.tick) {
				continue
			}
			var ids []int64
			if v.FullMembership {
				ids = slices.Clone(v.Added)
				slices.Sort(ids)
				ids = slices.Compact(ids)
			} else {
				if !ok || old.epoch != v.Epoch || old.tick != v.BaseTick {
					gap.views = append(gap.views, v.Observer)
					continue
				}
				ids = applyMembership(old.ids, v.Added, v.Removed)
			}
			if len(ids) > n.cfg.MaxViewEntities {
				return nil, errBadMessage
			}
			views[key] = viewBaseline{epoch: v.Epoch, tick: v.Tick, ids: ids}
			w := *v
			w.Entities = make([]EntityState, 0, len(ids))
			complete := true
			for _, id := range ids {
				s, ok := get(id)
				if !ok {
					complete = false
					missing(id)
					continue
				}
				w.Entities = append(w.Entities, s)
			}
			if !complete {
				continue // 不交付缺成员的视野：下一份带上全量的那个对象后再交付
			}
			m := *input
			m.Witness = &w
			m.Witnesses = nil
			m.Replica = false
			m.Deltas = nil
			messages = append(messages, &m)
		}
	}
	for k, v := range updates {
		c.states[k] = v
	}
	for id := range broken {
		delete(c.states, replicaKey{input.From, input.Kind, id})
	}
	for k, v := range views {
		c.views[k] = v
	}
	if input.Kind == kindGhost {
		for _, r := range input.Ghost.Removes {
			key := replicaKey{input.From, kindGhost, r.ID}
			if old, ok := c.states[key]; ok && !old.state.Ver.Newer(r.Ver) {
				delete(c.states, key)
			}
		}
	}
	c.bound(n.cfg.ReplicaCacheSize)
	if len(gap.states)+len(gap.views) > 0 {
		return messages, gap
	}
	return messages, nil
}

// 基线成员表发布后只读；静止视野直接共享它，不为每名观察者每拍重建 map 和排序。
func applyMembership(old, added, removed []int64) []int64 {
	if len(added) == 0 && len(removed) == 0 {
		return old
	}
	members := make(map[int64]bool, len(old)+len(added))
	for _, id := range old {
		members[id] = true
	}
	for _, id := range removed {
		delete(members, id)
	}
	for _, id := range added {
		members[id] = true
	}
	ids := make([]int64, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// requestReplicaRepair 请求发送方忘掉缺口涉及的对象与观察者的基线。同一个对象 / 观察者每个刷新窗口最多请求一次：
// 修复到达之前它所在的后续消息还会报同样的缺口，不能每份都发请求；请求本身丢了，窗口过后再报时重发，周期全量兜底。
// 返回是否发出了请求。
func (n *Node) requestReplicaRepair(peer int32, gap *replicaGap, now time.Time) bool {
	c := &n.receivedReplica
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.resetAfter) >= n.cfg.ReplicaCacheSize {
		clear(c.resetAfter)
	}
	due := func(key resetKey) bool {
		if now.Before(c.resetAfter[key]) {
			return false
		}
		c.resetAfter[key] = now.Add(n.cfg.ReplicaRefresh)
		return true
	}
	req := &replicaReset{Kind: gap.kind}
	for _, id := range gap.states {
		if due(resetKey{peer: peer, kind: gap.kind, id: id}) {
			req.States = append(req.States, id)
		}
	}
	for _, observer := range gap.views {
		if due(resetKey{peer: peer, view: true, id: observer}) {
			req.Views = append(req.Views, observer)
		}
	}
	if len(req.States)+len(req.Views) == 0 {
		return false
	}
	n.enqueue(job{kind: jobSend, send: outgoing{sid: peer, msg: &wireMsg{Kind: kindReplicaReset, ReplicaReset: req}}})
	return true
}
