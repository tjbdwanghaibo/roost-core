package cellworld

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// DeathFact 是一个生命代次唯一的持久死亡事实，不是临时伤害去重记录。
// RewardBase=0 表示没有玩家奖励；有接收者时，只有其 strict 事务回执才能关闭投递。
// 业务不得在 DamageApplied 观察点发奖；应使用 NodeConfig.SettleDeath，在同一 Player 事务里改 DAO。
type DeathFact struct {
	ID         string      `json:"id" msgpack:"i"`
	Target     int64       `json:"target" msgpack:"t"`
	Life       uint64      `json:"life" msgpack:"l"`
	Hit        HitID       `json:"hit" msgpack:"h"`
	RewardBase int32       `json:"base" msgpack:"b"`
	ReplySID   int32       `json:"reply_sid" msgpack:"rs"`
	State      EntityState `json:"state" msgpack:"s"`
	Damage     int32       `json:"damage" msgpack:"d"`
	At         int64       `json:"at" msgpack:"a"`
}

func (f DeathFact) valid() bool {
	return f.ID != "" && len(f.ID) <= 128 && f.Target != 0 && f.Life > 0 && f.Hit.ID != "" && len(f.Hit.ID) <= 128 && f.Hit.Attacker != 0 && f.RewardBase >= 0 && f.State.ID == f.Target && f.State.HP == 0 && f.State.Life == f.Life
}

// DeathIntent 先进入有界持久索引，再 CAS 写 OwnerRecord 的事实；后者是结算生效点。
// 崩溃留下的意图由任何 Cell 继续完成，旧 Grant 已被换走时只能丢弃意图。
type DeathIntent struct {
	Grant OwnerGrant `json:"grant"`
	Fact  DeathFact  `json:"fact"`
}
type DeathBucket map[int64]DeathIntent

const deathBuckets uint32 = 128
const maxDeathBucketEntries = 4096

func deathBucket(actor int64) uint32 { return uint32(uint64(actor)^(uint64(actor)>>32)) % deathBuckets }

func newRedisDeaths(client versionstore.RedisClient, world string) (versionstore.Store[uint32, DeathBucket], error) {
	return versionstore.NewRedisStore(client, versionstore.RedisConfig[uint32, DeathBucket]{Prefix: "roost:cellworld:" + world + ":deaths:", KeyOf: func(k uint32) string { return strconv.FormatUint(uint64(k), 10) }, Codec: versionstore.JSONCodec[DeathBucket]{}})
}
func (r *OwnerRegistry) recordDeath(ctx context.Context, grant OwnerGrant, fact DeathFact) (OwnerRecord, error) {
	if !fact.valid() {
		return OwnerRecord{}, errors.New("cellworld: invalid death fact")
	}
	before, found, err := r.store.Get(ctx, fact.Target)
	if err != nil {
		return OwnerRecord{}, err
	}
	if !found || before.Value.Grant != grant || before.Value.Life != fact.Life || before.Value.Phase != ownerActive {
		return OwnerRecord{}, ErrOwnerConflict
	}
	if before.Value.Death.ID != "" {
		if before.Value.Death != fact {
			return OwnerRecord{}, ErrOwnerConflict
		}
		return before.Value, nil
	}
	_, _, err = r.deaths.Update(ctx, deathBucket(fact.Target), func(bucket DeathBucket, _ bool) (DeathBucket, bool, error) {
		if old, ok := bucket[fact.Target]; ok {
			if old.Fact == fact && old.Grant == grant {
				return bucket, false, nil
			}
			if old.Grant.Epoch >= grant.Epoch {
				return bucket, false, ErrOwnerConflict
			}
		} else if len(bucket) >= maxDeathBucketEntries {
			return bucket, false, errors.New("cellworld: death outbox is full")
		}
		next := maps.Clone(bucket)
		if next == nil {
			next = DeathBucket{}
		}
		next[fact.Target] = DeathIntent{Grant: grant, Fact: fact}
		return next, true, nil
	})
	if err != nil {
		return OwnerRecord{}, err
	}
	return r.finishDeath(ctx, DeathIntent{Grant: grant, Fact: fact})
}
func (r *OwnerRegistry) finishDeath(ctx context.Context, intent DeathIntent) (OwnerRecord, error) {
	fact := intent.Fact
	return r.update(ctx, fact.Target, func(row OwnerRecord) (OwnerRecord, bool, error) {
		if row.Death == fact {
			return row, false, nil
		}
		if row.Grant != intent.Grant || row.Life != fact.Life || row.Phase != ownerActive || row.Death.ID != "" {
			return row, false, ErrOwnerConflict
		}
		row.Death = fact
		row.DeathAcked = fact.RewardBase == 0
		return row, true, nil
	})
}
func (r *OwnerRegistry) ackDeath(ctx context.Context, actor int64, id string, from int32) (OwnerRecord, error) {
	row, err := r.update(ctx, actor, func(row OwnerRecord) (OwnerRecord, bool, error) {
		if row.Death.ID != id || row.Death.RewardBase != from {
			return row, false, ErrOwnerConflict
		}
		if row.DeathAcked {
			return row, false, nil
		}
		row.DeathAcked = true
		return row, true, nil
	})
	if err == nil {
		err = r.removeDeathIntent(ctx, actor, id)
	}
	return row, err
}
func (r *OwnerRegistry) removeDeathIntent(ctx context.Context, actor int64, id string) error {
	_, _, err := r.deaths.Update(ctx, deathBucket(actor), func(bucket DeathBucket, _ bool) (DeathBucket, bool, error) {
		if v, ok := bucket[actor]; !ok || v.Fact.ID != id {
			return bucket, false, nil
		}
		next := maps.Clone(bucket)
		delete(next, actor)
		return next, true, nil
	})
	return err
}

type deathAck struct {
	Target int64  `msgpack:"t"`
	ID     string `msgpack:"i"`
}
type receivedDeathAck struct {
	from int32
	ack  deathAck
}

// runDeaths 不依赖 Actor 是否还在内存。每拍扫描一个固定桶，单桶最多 4096 项且受 I/O 截止时间约束；
// 不存在进程内索引丢失就永远找不到 outbox 的窗口。重复投递由接收 Player 的持久高水位处理。
func (n *Node) runDeaths(ctx context.Context) {
	ticker := time.NewTicker(n.cfg.DeathScanPeriod)
	defer ticker.Stop()
	var bucket uint32
	for {
		select {
		case <-ctx.Done():
			return
		case ack := <-n.deathAcks:
			op, cancel := context.WithTimeout(ctx, n.cfg.OwnershipTimeout)
			_, err := n.cfg.Owners.ackDeath(op, ack.ack.Target, ack.ack.ID, ack.from)
			cancel()
			if err != nil {
				n.stats.deathErrors.Add(1)
			}
			n.deathPending.Add(-1)
		case <-ticker.C:
			n.lifecycle.Lock()
			stopped := n.lifecycle.stopping
			if !stopped {
				n.deathPending.Add(1)
			}
			n.lifecycle.Unlock()
			if stopped {
				continue
			}
			if n.checkLease() != nil {
				n.deathPending.Add(-1)
				continue
			}
			op, cancel := context.WithTimeout(ctx, n.cfg.OwnershipTimeout)
			n.scanDeaths(op, bucket)
			cancel()
			n.deathPending.Add(-1)
			bucket = (bucket + 1) % deathBuckets
		}
	}
}
func (n *Node) scanDeaths(ctx context.Context, bucket uint32) {
	items, found, err := n.cfg.Owners.deaths.Get(ctx, bucket)
	if err != nil {
		n.stats.deathErrors.Add(1)
		return
	}
	if !found {
		return
	}
	// 固定桶上限有界；循环在操作截止时停止，下一轮继续。所有意图都可重放。
	for actor, intent := range items.Value {
		if ctx.Err() != nil {
			return
		}
		row, err := n.cfg.Owners.finishDeath(ctx, intent)
		if errors.Is(err, ErrOwnerConflict) {
			_ = n.cfg.Owners.removeDeathIntent(ctx, actor, intent.Fact.ID)
			continue
		}
		if err != nil {
			n.stats.deathErrors.Add(1)
			continue
		}
		if row.DeathAcked {
			_ = n.cfg.Owners.removeDeathIntent(ctx, actor, intent.Fact.ID)
			continue
		}
		n.enqueue(job{kind: jobSend, send: outgoing{sid: row.Death.RewardBase, msg: &wireMsg{Kind: kindDeath, Death: &row.Death}}})
	}
}

func (n *Node) handleDeath(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	p, err := playerOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	fact := *m.Death
	if !fact.valid() || fact.RewardBase != n.cfg.SID || n.cfg.PlayerEntity(fact.Hit.Attacker) != p.ID() {
		return nil, errors.New("cellworld: wrong death recipient")
	}
	if p.CellDeathReceipt(fact.Target) < fact.Life {
		if err := p.RecordCellDeathReceipt(fact.Target, fact.Life, n.cfg.MaxDeathReceipts); err != nil {
			return nil, err
		}
		if n.cfg.SettleDeath != nil {
			if err := n.cfg.SettleDeath(p, fact); err != nil {
				return nil, err
			}
		}
	}
	fx := &txEffects{}
	fx.send(m.From, &wireMsg{Kind: kindDeathAck, DeathAck: &deathAck{Target: fact.Target, ID: fact.ID}})
	n.registerCommit(fx)
	return nil, nil
}

func (n *Node) completeDeath(r *Region, a Actor, st ActorState, row OwnerRecord, now time.Time, fx *txEffects) {
	if st.Death == nil || row.Death.ID != st.Death.ID {
		return
	}
	wasPending := st.Role == RoleDying
	st.Death = &row.Death
	st.DeathAcked = row.DeathAcked
	st.Role = RoleReal
	st.HP = 0
	st.DeathNextAt = now.Add(n.cfg.DeathRetry).UnixNano()
	st.Ver.Seq++
	if row.DeathAcked && (st.Kind == KindNPC || st.CloseEntry) {
		st.Role = RoleGone
		a.SetCellState(st)
		n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true}}, now, &fx.sends)
		fx.owners = append(fx.owners, n.ownerWork(st))
		n.realChanged(st.ID, st.Ver.Epoch, false, fx)
	} else {
		a.SetCellState(st)
		n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleReal}}, now, &fx.sends)
		if !row.DeathAcked {
			fx.send(row.Death.RewardBase, &wireMsg{Kind: kindDeath, Death: &row.Death})
		}
	}
	if row.Death.ReplySID != 0 {
		fx.send(row.Death.ReplySID, &wireMsg{Kind: kindDamageReply, DamageReply: &damageReply{Hit: row.Death.Hit, Target: st.ID, Outcome: OutcomeApplied, HP: 0, Damage: row.Death.Damage}})
	}
	if wasPending {
		n.stats.deathsCommitted.Add(1)
		if hook := n.cfg.Hooks.DamageApplied; hook != nil {
			fx.hooks = append(fx.hooks, func() { hook(n.cell, st.ID, row.Death.Hit, 0) })
		}
	}
}

func missingDeathStore() error {
	return fmt.Errorf("%w: a durable death outbox store is required", ErrNodeConfig)
}
