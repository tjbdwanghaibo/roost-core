package cellworld

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

var (
	ErrOwnerConflict        = errors.New("cellworld: ownership changed")
	ErrOwnerClosed          = errors.New("cellworld: entry is closed")
	errOwnerObservedChanged = errors.New("cellworld: ownership changed during liveness check")
)

// OwnerProcess 标识一次进程启动，不能只比较 SID。正式接线使用 App 的单实例锁令牌。
type OwnerProcess struct {
	SID         int32  `json:"sid" msgpack:"s"`
	Incarnation string `json:"incarnation" msgpack:"i"`
}

func (p OwnerProcess) valid() bool {
	return p.SID > 0 && p.Incarnation != "" && len(p.Incarnation) <= 128
}

// OwnerGrant 是一次写授权。ID 跨重试不变，Epoch 在共享记录里分配，不能由 Cell 的局部表分配。
type OwnerGrant struct {
	ID      string       `json:"id" msgpack:"i"`
	Epoch   uint64       `json:"epoch" msgpack:"e"`
	Process OwnerProcess `json:"process" msgpack:"p"`
}

type ownerPhase uint8

const (
	ownerActive ownerPhase = iota + 1
	ownerFrozen
	ownerClosed
)

// OwnerTransfer 的 Epoch 同时是交接请求的高水位。拒绝以后仍保留，迟到 Begin 不会重新冻结授权。
type OwnerTransfer struct {
	ID     string `json:"id" msgpack:"i"`
	Target int32  `json:"target" msgpack:"t"`
	Epoch  uint64 `json:"epoch" msgpack:"e"`
}

// OwnerRecord 只存授权，不存每拍状态。记录不设 TTL、不删除：关闭后保留代际和入场高水位。
// Phase=frozen 的前提是源 Cell 已在 Nest 事务内停止写入；只有目标能接受或拒绝这次交接。
type OwnerRecord struct {
	Birth       BirthRecord   `json:"birth"`
	Life        uint64        `json:"life"`
	Death       DeathFact     `json:"death"`
	DeathAcked  bool          `json:"death_acked"`
	Actor       int64         `json:"actor"`
	Kind        EntityKind    `json:"kind"`
	Base        int32         `json:"base"`
	Entry       uint64        `json:"entry"`
	ClosedEntry uint64        `json:"closed_entry"`
	Grant       OwnerGrant    `json:"grant"`
	Phase       ownerPhase    `json:"phase"`
	Transfer    OwnerTransfer `json:"transfer"`
}

// OwnerRegistry 串行化入场、交接和退役。方法会做 I/O，只能在实体锁外调用。
// alive 必须核对具体进程代次；查询错误是未知，不是死亡。恢复还依赖旧进程失锁后的 fail-stop。
type OwnerRegistry struct {
	store  versionstore.Store[int64, OwnerRecord]
	deaths versionstore.Store[uint32, DeathBucket]
	alive  func(context.Context, OwnerProcess) (bool, error)
}

func NewOwnerRegistry(store versionstore.Store[int64, OwnerRecord], alive func(context.Context, OwnerProcess) (bool, error), deaths versionstore.Store[uint32, DeathBucket]) (*OwnerRegistry, error) {
	if store == nil || alive == nil {
		return nil, fmt.Errorf("%w: ownership store and incarnation checker are required", ErrNodeConfig)
	}
	if deaths == nil {
		return nil, missingDeathStore()
	}
	return &OwnerRegistry{store: store, alive: alive, deaths: deaths}, nil
}

// NewRedisOwnerRegistry 使用现有 CAS 存储；所有 Cell / Base 必须使用同一 world 前缀，不能设 TTL。
func NewRedisOwnerRegistry(client versionstore.RedisClient, world string, alive func(context.Context, OwnerProcess) (bool, error)) (*OwnerRegistry, error) {
	if !validWorld(world) {
		return nil, fmt.Errorf("%w: invalid ownership world", ErrNodeConfig)
	}
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int64, OwnerRecord]{
		Prefix: "roost:cellworld:" + world + ":owner:", KeyOf: func(id int64) string { return strconv.FormatInt(id, 10) },
		Codec: versionstore.JSONCodec[OwnerRecord]{},
	})
	if err != nil {
		return nil, err
	}
	deaths, err := newRedisDeaths(client, world)
	if err != nil {
		return nil, err
	}
	return NewOwnerRegistry(store, alive, deaths)
}

type ownerClaim struct {
	Generation uint64
	Actor      int64
	Kind       EntityKind
	Base       int32
	Entry      uint64
	Process    OwnerProcess
	Token      string
	Epoch      uint64 // 不低于调用方已经见过的版本，恢复后旧快照不覆盖新状态。
}

func (c ownerClaim) valid() bool {
	if c.Actor == 0 || !c.Process.valid() || c.Token == "" || len(c.Token) > 128 {
		return false
	}
	switch c.Kind {
	case KindPlayer:
		return c.Base > 0 && c.Entry > 0
	case KindNPC:
		return c.Base == 0 && c.Entry == 0
	default:
		return false
	}
}

// claim 已有活写者时返回它的记录，调用方只能转向它，不能装出另一个 Real。
// 活性查询在 CAS 外执行；CAS 内再次核对整份授权身份，查询期间发生交接就重新读取。
func (r *OwnerRegistry) claim(ctx context.Context, req ownerClaim) (OwnerRecord, error) {
	if !req.valid() {
		return OwnerRecord{}, fmt.Errorf("%w: invalid claim", ErrOwnerConflict)
	}
	for range versionstore.DefaultMaxAttempts {
		if err := ctx.Err(); err != nil {
			return OwnerRecord{}, err
		}
		before, found, err := r.store.Get(ctx, req.Actor)
		if err != nil {
			return OwnerRecord{}, err
		}
		old := before.Value
		if found && !old.valid(req.Actor) {
			return OwnerRecord{}, versionstore.ErrMalformedRecord
		}
		dead := false
		if found && old.Phase != ownerClosed && old.Grant.Process != req.Process {
			live, err := r.alive(ctx, old.Grant.Process)
			if err != nil {
				return OwnerRecord{}, err
			}
			dead = !live
		}
		result, _, err := r.store.Update(ctx, req.Actor, func(current OwnerRecord, exists bool) (OwnerRecord, bool, error) {
			if exists != found || current != old {
				return current, false, errOwnerObservedChanged
			}
			if exists && current.Actor != req.Actor {
				return current, false, versionstore.ErrMalformedRecord
			}
			if exists && current.Birth.Generation != req.Generation {
				return current, false, ErrOwnerConflict
			}
			if exists && (current.Kind != req.Kind || current.Base != req.Base || req.Entry < current.Entry) {
				return current, false, ErrOwnerConflict
			}
			if req.Kind == KindPlayer && req.Entry <= current.ClosedEntry {
				return current, false, ErrOwnerClosed
			}
			if exists && req.Entry > current.Entry && current.Death.ID != "" && !current.DeathAcked {
				return current, false, ErrOwnerConflict
			}
			if exists && current.Phase != ownerClosed && !dead {
				if current.Grant.Process == req.Process && current.Phase == ownerActive && req.Entry > current.Entry {
					epoch := max(current.Grant.Epoch, current.Transfer.Epoch, req.Epoch)
					if epoch == ^uint64(0) {
						return current, false, ErrOwnerConflict
					}
					current.Entry = req.Entry
					current.Grant.Epoch = epoch + 1
					// 新的登录 Entry 不是复活；保留生命代次与死亡事实。
					return current, true, nil
				}
				return current, false, nil
			}
			// 同一个已退役授权的迟到重试不能复活；新入场 / 新一次明确 Spawn 使用新令牌。
			if exists && current.Grant.ID == req.Token {
				return current, false, ErrOwnerClosed
			}
			epoch := max(current.Grant.Epoch, current.Transfer.Epoch)
			if epoch == ^uint64(0) {
				return current, false, fmt.Errorf("%w: epoch exhausted", ErrOwnerConflict)
			}
			next := OwnerRecord{Actor: req.Actor, Kind: req.Kind, Base: req.Base, Entry: req.Entry, ClosedEntry: current.ClosedEntry,
				Grant: OwnerGrant{ID: req.Token, Epoch: max(epoch+1, req.Epoch), Process: req.Process}, Phase: ownerActive}
			next.Birth = current.Birth
			next.Life = current.Life
			next.Death, next.DeathAcked = current.Death, current.DeathAcked
			if !exists || req.Kind == KindNPC && current.Phase == ownerClosed && (current.Death.ID == "" || current.DeathAcked) {
				next.Life = next.Grant.Epoch
				next.Death = DeathFact{}
				next.DeathAcked = false
			}
			return next, true, nil
		})
		if errors.Is(err, errOwnerObservedChanged) {
			continue
		}
		return result.Value, err
	}
	return OwnerRecord{}, versionstore.ErrConflict
}

func (r *OwnerRegistry) begin(ctx context.Context, actor int64, grant OwnerGrant, transfer OwnerTransfer) (OwnerRecord, error) {
	if transfer.ID == "" || transfer.Target <= 0 || transfer.Target == grant.Process.SID || transfer.Epoch <= grant.Epoch {
		return OwnerRecord{}, ErrOwnerConflict
	}
	return r.update(ctx, actor, func(current OwnerRecord) (OwnerRecord, bool, error) {
		if current.Transfer == transfer {
			return current, false, nil // 已受理、已拒绝或已接受：重传不改结论。
		}
		if current.Grant != grant || current.Phase != ownerActive || transfer.Epoch <= current.Transfer.Epoch {
			return current, false, ErrOwnerConflict
		}
		current.Phase, current.Transfer = ownerFrozen, transfer
		return current, true, nil
	})
}

func (r *OwnerRegistry) accept(ctx context.Context, actor int64, source OwnerGrant, transfer OwnerTransfer, target OwnerProcess) (OwnerRecord, error) {
	if !target.valid() || target.SID != transfer.Target {
		return OwnerRecord{}, ErrOwnerConflict
	}
	return r.update(ctx, actor, func(current OwnerRecord) (OwnerRecord, bool, error) {
		if current.Transfer == transfer && current.Phase == ownerActive && current.Grant.ID == transfer.ID && current.Grant.Process == target {
			return current, false, nil
		}
		if current.Grant != source || current.Phase != ownerFrozen || current.Transfer != transfer {
			return current, false, ErrOwnerConflict
		}
		current.Grant = OwnerGrant{ID: transfer.ID, Epoch: transfer.Epoch, Process: target}
		current.Phase = ownerActive
		return current, true, nil
	})
}

func (r *OwnerRegistry) reject(ctx context.Context, actor int64, source OwnerGrant, transfer OwnerTransfer, target OwnerProcess) (OwnerRecord, error) {
	if !target.valid() || target.SID != transfer.Target {
		return OwnerRecord{}, ErrOwnerConflict
	}
	return r.update(ctx, actor, func(current OwnerRecord) (OwnerRecord, bool, error) {
		if current.Grant != source || current.Transfer != transfer || current.Phase == ownerClosed {
			return current, false, ErrOwnerConflict
		}
		if current.Phase == ownerActive {
			return current, false, nil
		}
		current.Phase = ownerActive
		return current, true, nil
	})
}

// retire 只能在本地实例提交 Gone 后调用。与 Accept 竞争时最多一个 CAS 生效；不能关闭目标的新授权。
func (r *OwnerRegistry) retire(ctx context.Context, actor int64, grant OwnerGrant) (OwnerRecord, error) {
	return r.retireEntry(ctx, actor, grant, 0)
}

func (r *OwnerRegistry) retireEntry(ctx context.Context, actor int64, grant OwnerGrant, closedEntry uint64) (OwnerRecord, error) {
	return r.update(ctx, actor, func(current OwnerRecord) (OwnerRecord, bool, error) {
		if current.Grant != grant {
			return current, false, ErrOwnerConflict
		}
		if current.Phase == ownerClosed && closedEntry <= current.ClosedEntry {
			return current, false, nil
		}
		current.Phase = ownerClosed
		current.ClosedEntry = max(current.ClosedEntry, closedEntry)
		current.Entry = max(current.Entry, closedEntry)
		return current, true, nil
	})
}

func (r *OwnerRegistry) update(ctx context.Context, actor int64, change func(OwnerRecord) (OwnerRecord, bool, error)) (OwnerRecord, error) {
	if err := ctx.Err(); err != nil {
		return OwnerRecord{}, err
	}
	result, _, err := r.store.Update(ctx, actor, func(current OwnerRecord, found bool) (OwnerRecord, bool, error) {
		if !found || current.Actor != actor {
			return current, false, ErrOwnerConflict
		}
		if !current.valid(actor) {
			return current, false, versionstore.ErrMalformedRecord
		}
		return change(current)
	})
	return result.Value, err
}

func (r OwnerRecord) valid(actor int64) bool {
	if r.Actor != actor || actor == 0 || r.Kind != KindPlayer && r.Kind != KindNPC {
		return false
	}
	if r.Kind == KindPlayer && (r.Base <= 0 || r.Entry == 0) {
		return false
	}
	if r.Kind == KindNPC && (r.Base != 0 || r.Entry != 0) {
		return false
	}
	if r.Life == 0 {
		return false
	}
	if r.Phase < ownerActive || r.Phase > ownerClosed || r.Grant.ID == "" || r.Grant.Epoch == 0 || !r.Grant.Process.valid() {
		return false
	}
	if r.Transfer == (OwnerTransfer{}) {
		return r.Phase != ownerFrozen
	}
	return r.Transfer.ID != "" && r.Transfer.Target > 0 && r.Transfer.Epoch > 0
}
