package cellworld

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// SpawnSpec 描述一个固定出生槽，State.ID 跨进程重启和重生不变。
// 活怪走到邻格仍占着原槽；只在死亡已确认且退役后，按 RespawnDelay 开启下一代。
// 配置修改必须显式换槽或换 world，不能静默把存活实例换成另一种怪。
type SpawnSpec struct {
	Key          string        `json:"key"`
	Home         int32         `json:"home"`
	State        EntityState   `json:"state"`
	RespawnDelay time.Duration `json:"respawn_delay"`
}

func (s SpawnSpec) valid() bool {
	return validWorld(s.Key) && s.Home > 0 && s.State.ID != 0 && s.State.Kind == KindNPC && s.State.Base == 0 && s.State.HP > 0 && s.State.Life == 0 && s.State.Ver == (Version{}) && s.RespawnDelay >= 0
}

// BirthRecord 与写授权共用一行 CAS。Generation 是槽的实例代次，Life 是战斗生命代次；
// 重启/跨格只换 Grant，不推进 Generation。Waiting/RespawnAt 是死亡后的持久倒计时。
type BirthRecord struct {
	Spec       SpawnSpec `json:"spec"`
	Generation uint64    `json:"generation"`
	LastDeath  uint64    `json:"last_death"`
	Waiting    bool      `json:"waiting"`
	RespawnAt  int64     `json:"respawn_at"`
}

// prepareSpawn 预留或对账出生槽，只返回准入计划。真正写权仍由 claim 在异步 I/O 上 CAS。
func (r *OwnerRegistry) prepareSpawn(ctx context.Context, spec SpawnSpec, now time.Time) (OwnerRecord, error) {
	if !spec.valid() {
		return OwnerRecord{}, errors.New("cellworld: invalid spawn slot")
	}
	result, _, err := r.store.Update(ctx, spec.State.ID, func(row OwnerRecord, exists bool) (OwnerRecord, bool, error) {
		if !exists {
			row = OwnerRecord{Actor: spec.State.ID, Kind: KindNPC, Life: 1, Phase: ownerClosed, Grant: OwnerGrant{ID: "reserved-" + spec.Key, Epoch: 1, Process: OwnerProcess{SID: spec.Home, Incarnation: "reserved"}}, Birth: BirthRecord{Spec: spec, Generation: 1}}
			return row, true, nil
		}
		if !row.valid(spec.State.ID) || row.Birth.Spec != spec {
			return row, false, errors.New("cellworld: spawn slot identity or configuration changed")
		}
		changed := false
		if row.Death.ID != "" && row.DeathAcked && row.Birth.LastDeath < row.Death.Life {
			row.Birth.LastDeath = row.Death.Life
			row.Birth.Waiting = true
			row.Birth.RespawnAt = row.Death.At + spec.RespawnDelay.Milliseconds()
			changed = true
		}
		if row.Phase == ownerClosed && row.Birth.Waiting && now.UnixMilli() >= row.Birth.RespawnAt {
			if row.Birth.Generation == ^uint64(0) {
				return row, false, errors.New("cellworld: spawn generation exhausted")
			}
			row.Birth.Generation++
			row.Birth.Waiting = false
			row.Birth.RespawnAt = 0
			changed = true
		}
		return row, changed, nil
	})
	return result.Value, err
}

type spawnRequest struct {
	State      EntityState
	Generation uint64
}

// ReconcileSpawn 在锁外调用；成功只表示当前槽无需动作或 Pending 已准入。
// shared store / 活性未知时返回错误，不补刷、不夺权。调用者周期重试，不能在 Nest handler 里等待。
func (n *Node) ReconcileSpawn(ctx context.Context, spec SpawnSpec) error {
	if nest.CurrentRollbackTx() != nil {
		return errors.New("cellworld: spawn reconciliation must run outside Nest")
	}
	if !n.hostsCell || spec.Home != n.cfg.SID || !n.rect.Contains(spec.State.Pos()) {
		return errors.New("cellworld: spawn slot belongs to another home")
	}
	if err := n.checkLease(); err != nil {
		return err
	}
	row, err := n.cfg.Owners.prepareSpawn(ctx, spec, time.Now())
	if err != nil {
		return err
	}
	if row.Birth.Waiting {
		// 死亡已确认但记录未关闭：owner 是本进程或仍活着时由它的实例退役；owner 已死（确认后、退役前崩溃）
		// 时代为关闭，随后按绝对复活时间推进下一代。活性未知返回错误，下一轮重试。
		if row.Phase == ownerClosed || row.Grant.Process == n.ownerProcess() {
			return nil
		}
		if _, err := n.cfg.Owners.closeAbandoned(ctx, spec.State.ID); err != nil {
			if errors.Is(err, ErrOwnerConflict) {
				return nil
			}
			return err
		}
		if row, err = n.cfg.Owners.prepareSpawn(ctx, spec, time.Now()); err != nil {
			return err
		}
		if row.Birth.Waiting {
			return nil
		}
	}
	if row.Phase != ownerClosed {
		if row.Grant.Process == n.ownerProcess() {
			return nil
		}
		live, err := n.cfg.Owners.alive(ctx, row.Grant.Process)
		if err != nil {
			return err
		}
		if live {
			return nil
		}
	}
	_, err = n.cfg.Nest.RequestMulti(ctx, hSpawn, []int64{n.cfg.Scene, spec.State.ID}, nest.Params{spawnRequest{State: spec.State, Generation: row.Birth.Generation}})
	return err
}

// RunSpawns 是有界的后台巡检，不占 Nest 业务 worker。每 100ms 顺序处理至多 64 个槽，
// 单项有 OwnershipTimeout；不在本地记录 Alive/Dead。停止时返回，调用者先取消并等待它再 Drain Node。
func (n *Node) RunSpawns(ctx context.Context, specs []SpawnSpec, onError func(SpawnSpec, error)) error {
	specs = append([]SpawnSpec(nil), specs...)
	for _, spec := range specs {
		if !spec.valid() || spec.Home != n.cfg.SID || !n.rect.Contains(spec.State.Pos()) {
			return errors.New("cellworld: invalid spawn configuration")
		}
	}
	if len(specs) == 0 {
		return nil
	}
	cursor := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for range min(64, len(specs)) {
			if err := ctx.Err(); err != nil {
				return err
			}
			spec := specs[cursor]
			cursor = (cursor + 1) % len(specs)
			op, cancel := context.WithTimeout(ctx, n.cfg.OwnershipTimeout)
			err := n.ReconcileSpawn(op, spec)
			cancel()
			if err != nil {
				n.stats.spawnErrors.Add(1)
				if onError != nil {
					onError(spec, err)
				}
			} else {
				n.stats.spawnChecks.Add(1)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// SpawnCatalog 仅在配置装载时更新，一次 CAS 预留整批名字；周期巡检不改它。
// 名字到序号永不回收，避免重启、配置重排或添加出生点复用仍在邻格的 ID。
type SpawnCatalog struct {
	Next  uint64            `json:"next"`
	Slots map[string]uint64 `json:"slots"`
}
type SpawnBook struct {
	store versionstore.Store[int32, SpawnCatalog]
}

func NewSpawnBook(store versionstore.Store[int32, SpawnCatalog]) (*SpawnBook, error) {
	if store == nil {
		return nil, errors.New("cellworld: spawn catalog store is required")
	}
	return &SpawnBook{store: store}, nil
}
func NewRedisSpawnBook(client versionstore.RedisClient, world string) (*SpawnBook, error) {
	if !validWorld(world) {
		return nil, errors.New("cellworld: invalid spawn world")
	}
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int32, SpawnCatalog]{Prefix: "roost:cellworld:" + world + ":birth:", KeyOf: func(home int32) string { return strconv.Itoa(int(home)) }, Codec: versionstore.JSONCodec[SpawnCatalog]{}})
	if err != nil {
		return nil, err
	}
	return NewSpawnBook(store)
}
func (b *SpawnBook) Reserve(ctx context.Context, home int32, keys []string) (map[string]uint64, error) {
	if home <= 0 || len(keys) > 50000 {
		return nil, errors.New("cellworld: invalid spawn catalog size")
	}
	requested := map[string]bool{}
	for _, key := range keys {
		if !validWorld(key) || requested[key] {
			return nil, fmt.Errorf("cellworld: invalid or repeated spawn key %q", key)
		}
		requested[key] = true
	}
	row, _, err := b.store.Update(ctx, home, func(old SpawnCatalog, _ bool) (SpawnCatalog, bool, error) {
		// 删除配置不等于删除活怪；必须先显式排空或换 world，不偷偷减少/转移配额。
		for key := range old.Slots {
			if !requested[key] {
				return old, false, fmt.Errorf("cellworld: live catalog slot %q is missing from configuration", key)
			}
		}
		next := SpawnCatalog{Next: old.Next, Slots: maps.Clone(old.Slots)}
		if next.Slots == nil {
			next.Slots = map[string]uint64{}
		}
		changed := false
		for _, key := range keys {
			if next.Slots[key] != 0 {
				continue
			}
			if next.Next >= 50000 {
				return old, false, errors.New("cellworld: spawn catalog exhausted")
			}
			next.Next++
			next.Slots[key] = next.Next
			changed = true
		}
		return next, changed, nil
	})
	if err != nil {
		return nil, err
	}
	return maps.Clone(row.Value.Slots), nil
}
