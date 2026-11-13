package cellworld

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// 内部复制与客户端 EntitySync 分离。队列只合并完整状态；发送时比较上次发出的
// 基线，接收方必须精确匹配 base version。缺口丢弃整批并请求全量，绝不猜差值。
// 缓存可丢弃、总量有界，不是业务权威。周期全量兜住恢复请求也丢失的情况。
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
	resetAfter map[int32]time.Time
}

func (c *replicaCache) init() {
	c.states = make(map[replicaKey]stateBaseline)
	c.views = make(map[replicaKey]viewBaseline)
	c.resetAfter = make(map[int32]time.Time)
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
			full := !ok || old.epoch != w.Epoch || !now.Before(old.fullAt.Add(n.cfg.ReplicaRefresh))
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

// decodeReplica 在网络回调里只重建值副本；整个批次验证后才推进缓存。
// 真正业务状态仍由各自 Nest handler 校验版本并提交。
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
	updates := make(map[replicaKey]stateBaseline, len(input.Deltas))
	get := func(id int64) (EntityState, bool) {
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
		if !ok {
			return nil, errReplicaGap
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
			state, _ := get(d.ID)
			b.Updates = append(b.Updates, state)
		}
		messages = append(messages, &m)
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
					return nil, errReplicaGap
				}
				ids = applyMembership(old.ids, v.Added, v.Removed)
			}
			if len(ids) > n.cfg.MaxViewEntities {
				return nil, errBadMessage
			}
			w := *v
			w.Entities = make([]EntityState, 0, len(ids))
			for _, id := range ids {
				s, ok := get(id)
				if !ok {
					return nil, errReplicaGap
				}
				w.Entities = append(w.Entities, s)
			}
			m := *input
			m.Witness = &w
			m.Witnesses = nil
			m.Replica = false
			m.Deltas = nil
			messages = append(messages, &m)
			views[key] = viewBaseline{epoch: v.Epoch, tick: v.Tick, ids: ids}
		}
	}
	for k, v := range updates {
		c.states[k] = v
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

// 同一丢包缺口可能影响几百份视野；只请求一次修复，避免每份失败包都清掉发送基线形成全量风暴。
func (n *Node) requestReplicaReset(peer int32, now time.Time) bool {
	c := &n.receivedReplica
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.resetAfter[peer]) {
		return false
	}
	if len(c.resetAfter) >= n.cfg.ReplicaCacheSize {
		clear(c.resetAfter)
	}
	c.resetAfter[peer] = now.Add(n.cfg.ReplicaRefresh)
	n.enqueue(job{kind: jobSend, send: outgoing{sid: peer, msg: &wireMsg{Kind: kindReplicaReset}}})
	return true
}
