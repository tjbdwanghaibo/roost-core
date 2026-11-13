package cellworld

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// ErrNodeStopping 表示 Node 已停止新业务准入；Drain 超时后仍保持关闭，调用方应保留依赖并继续等。
var ErrNodeStopping = errors.New("cellworld: node is draining")

// networkQueue 保存完整状态，只有真正取出发送时才做增量编码。
// 同一目标、种类、实体占一个位置；更新不改变排队位置，防止热点饿死其他实体。
// 删除与更新共用 Ghost 键，版本相同时删除优先，不能把墓碑覆盖成旧 create。
type stateKey struct {
	sid  int32
	kind msgKind
	id   int64
}
type queuedSend struct {
	out outgoing
	at  time.Time
}
type networkQueue struct {
	mu     sync.Mutex
	states map[stateKey]queuedSend
	keys   []stateKey
	head   int
	ready  chan struct{}
}

func (q *networkQueue) init() {
	q.states = make(map[stateKey]queuedSend)
	q.ready = make(chan struct{}, 1)
}
func (q *networkQueue) put(key stateKey, send outgoing, limit int) (accepted, merged bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if old, ok := q.states[key]; ok {
		if newerState(send.msg, old.out.msg) {
			old.out = send
			q.states[key] = old
		}
		return true, true
	}
	if len(q.states) >= limit {
		return false, false
	}
	q.states[key] = queuedSend{out: send, at: time.Now()}
	q.keys = append(q.keys, key)
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true, false
}
func (q *networkQueue) take(limit int) []queuedSend {
	q.mu.Lock()
	defer q.mu.Unlock()
	var batch []queuedSend
	for q.head < len(q.keys) && len(batch) < limit {
		key := q.keys[q.head]
		q.head++
		batch = append(batch, q.states[key])
		delete(q.states, key)
	}
	if q.head == len(q.keys) {
		q.keys = q.keys[:0]
		q.head = 0
	} else {
		if q.head >= len(q.keys)/2 {
			copy(q.keys, q.keys[q.head:])
			q.keys = q.keys[:len(q.keys)-q.head]
			q.head = 0
		}
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
	return batch
}
func (q *networkQueue) len() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.states) }
func ghostVersion(m *wireMsg) (Version, bool) {
	if len(m.Ghost.Removes) > 0 {
		return m.Ghost.Removes[0].Ver, true
	}
	return m.Ghost.Updates[0].Ver, false
}
func newerState(next, old *wireMsg) bool {
	switch next.Kind {
	case kindGhost:
		nv, nr := ghostVersion(next)
		ov, or := ghostVersion(old)
		return nv.Newer(ov) || nv == ov && nr && !or
	case kindWitness:
		n, o := next.Witness, old.Witness
		return n.Epoch > o.Epoch || n.Epoch == o.Epoch && n.Tick > o.Tick
	case kindWriteback:
		return next.Writeback.State.Ver.Newer(old.Writeback.State.Ver)
	}
	return false
}

func (n *Node) enqueueSend(send outgoing) bool {
	put := func(id int64, m *wireMsg) bool {
		n.outputPending.Add(1)
		ok, merged := n.network.put(stateKey{send.sid, m.Kind, id}, outgoing{sid: send.sid, msg: m}, n.cfg.OutboxSize)
		if merged || !ok {
			n.outputPending.Add(-1)
		}
		if merged {
			n.stats.statesCoalesced.Add(1)
		}
		if !ok {
			n.stats.outboxDrops.Add(1)
		}
		return ok
	}
	switch m := send.msg; m.Kind {
	case kindGhost:
		ok := true
		for _, s := range m.Ghost.Updates {
			ok = put(s.ID, &wireMsg{Kind: kindGhost, SentAt: m.SentAt, Ghost: &ghostBatch{Updates: []EntityState{s}}}) && ok
		}
		for _, r := range m.Ghost.Removes {
			ok = put(r.ID, &wireMsg{Kind: kindGhost, SentAt: m.SentAt, Ghost: &ghostBatch{Removes: []ghostRemove{r}}}) && ok
		}
		return ok
	case kindWitness:
		return put(m.Witness.Observer, m)
	case kindWriteback:
		return put(m.Writeback.State.ID, m)
	default:
		n.outputPending.Add(1)
		select {
		case n.controls <- queuedSend{out: send, at: time.Now()}:
			return true
		default:
			n.outputPending.Add(-1)
			n.stats.outboxDrops.Add(1)
			return false
		}
	}
}

// 控制消息与状态各一个网络消费者，避免重试一笔控制消息阻塞全部状态。
// Publish 失败只在有限预算内重试；成功仅代表交给传输，业务仍负责对端丢包后的幂等重传。
func (n *Node) runControls(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-n.controls:
			n.observeQueueAge(item.at)
			for attempt := 0; attempt < n.cfg.PublishAttempts; attempt++ {
				if n.publish(item.out) == nil {
					break
				}
				if attempt+1 == n.cfg.PublishAttempts {
					n.stats.controlExhausted.Add(1)
					break
				}
				n.stats.controlRetries.Add(1)
				timer := time.NewTimer(n.cfg.PublishRetry)
				select {
				case <-ctx.Done():
					timer.Stop()
					n.outputPending.Add(-1)
					return
				case <-timer.C:
				}
			}
			n.outputPending.Add(-1)
		}
	}
}
func (n *Node) runStates(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.network.ready:
			batch := n.network.take(n.cfg.GhostChunk)
			ghosts := map[int32]*wireMsg{}
			witnesses := map[int32]*wireMsg{}
			flushViews := func(sid int32) {
				if m := witnesses[sid]; m != nil {
					_ = n.publish(outgoing{sid: sid, msg: m})
					delete(witnesses, sid)
				}
			}
			for _, item := range batch {
				n.observeQueueAge(item.at)
				if item.out.msg.Kind == kindWitness {
					sid := item.out.sid
					m := witnesses[sid]
					if m == nil {
						copy := *item.out.msg
						m = &copy
						witnesses[sid] = m
					} else {
						m.Witnesses = append(m.Witnesses, item.out.msg.Witness)
					}
					if len(m.Witnesses)+1 >= n.cfg.WitnessBatch {
						flushViews(sid)
					}
					continue
				}
				if item.out.msg.Kind != kindGhost {
					_ = n.publish(item.out)
					continue
				}
				m := ghosts[item.out.sid]
				if m == nil {
					m = &wireMsg{Kind: kindGhost, SentAt: item.out.msg.SentAt, Ghost: &ghostBatch{}}
					ghosts[item.out.sid] = m
				}
				m.Ghost.Updates = append(m.Ghost.Updates, item.out.msg.Ghost.Updates...)
				m.Ghost.Removes = append(m.Ghost.Removes, item.out.msg.Ghost.Removes...)
			}
			for sid := range witnesses {
				flushViews(sid)
			}
			for sid, m := range ghosts {
				_ = n.publish(outgoing{sid: sid, msg: m})
			}
			n.outputPending.Add(-int64(len(batch)))
		}
	}
}
func (n *Node) observeQueueAge(at time.Time) {
	age := uint64(max(0, time.Since(at).Nanoseconds()))
	for old := n.stats.queueAgeMax.Load(); age > old; old = n.stats.queueAgeMax.Load() {
		if n.stats.queueAgeMax.CompareAndSwap(old, age) {
			break
		}
	}
}

// 销毁只在独立 I/O worker 上执行，同一实体去重；失败后由 Gone 心跳重新提交。
func (n *Node) enqueueDestroy(j job) bool {
	n.destroyMu.Lock()
	defer n.destroyMu.Unlock()
	if n.destroyBusy[j.entity] {
		return true
	}
	select {
	case n.destroyQueue <- j:
		n.destroyBusy[j.entity] = true
		return true
	default:
		n.stats.outboxDrops.Add(1)
		return false
	}
}
func (n *Node) runDestroys(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-n.destroyQueue:
			opctx, cancel := context.WithTimeout(ctx, n.cfg.OwnershipTimeout)
			if err := n.cfg.Destroy(opctx, j.entity); err != nil {
				n.stats.destroyErrors.Add(1)
			}
			cancel()
			n.destroyMu.Lock()
			delete(n.destroyBusy, j.entity)
			n.destroyMu.Unlock()
		}
	}
}

// 一个事务的生命周期票据在提交全部效果之后，或回滚时归还。这里只记录资源生命周期，
// 不保存玩法状态；同一事务从多个 Cell API 进入只登记一次。
type nodeTransaction struct {
	node *Node
	tx   *nest.RollbackTx
}

func (g *nodeTransaction) finish() {
	g.node.lifecycle.Lock()
	delete(g.node.lifecycle.transactions, g.tx)
	g.node.lifecycle.Unlock()
}
func (g *nodeTransaction) PrepareCommit(tx *nest.RollbackTx) error {
	if err := g.node.checkLease(); err != nil {
		return err
	}
	tx.AfterCommit(g.finish)
	return nil
}
func (n *Node) beginTransaction() error {
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	tx := nest.CurrentRollbackTx()
	if _, ok := n.lifecycle.transactions[tx]; ok {
		return nil
	}
	if n.lifecycle.stopping {
		return ErrNodeStopping
	}
	if tx == nil {
		return nil
	}
	if n.lifecycle.transactions == nil {
		n.lifecycle.transactions = make(map[*nest.RollbackTx]bool)
	}
	n.lifecycle.transactions[tx] = true
	g := &nodeTransaction{node: n, tx: tx}
	if err := tx.RegisterCommitParticipant(g); err != nil {
		delete(n.lifecycle.transactions, tx)
		return err
	}
	tx.DeferRollback(func() error { g.finish(); return nil })
	return nil
}

// Drain 停止新业务与收件，等待已提交效果、本地收尾和正在运行的事务完成。
// 不取消底层操作、不关闭传输；超时后保留依赖、用新 context 再调。成功后才取消 Run。
func (n *Node) Drain(ctx context.Context) error {
	if err := n.CloseIngress(ctx); err != nil {
		return err
	}
	n.lifecycle.Lock()
	n.lifecycle.stopping = true
	n.lifecycle.Unlock()
	ticker := time.NewTicker(time.Millisecond * 10)
	defer ticker.Stop()
	for {
		n.lifecycle.Lock()
		active := len(n.lifecycle.transactions)
		n.lifecycle.Unlock()
		n.ownerMu.Lock()
		owners := len(n.ownerBusy)
		n.ownerMu.Unlock()
		n.destroyMu.Lock()
		destroying := len(n.destroyBusy)
		n.destroyMu.Unlock()
		if active == 0 && owners == 0 && destroying == 0 && n.deathPending.Load() == 0 && n.outputPending.Load() == 0 && len(n.out) == 0 && len(n.controls) == 0 && n.network.len() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
