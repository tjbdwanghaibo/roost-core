package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// AdminCommandEntityUnload 是运维卸载单个实体的 admin 命令名（D16）。
const AdminCommandEntityUnload = "entity.unload"

// DefaultEntityUnloadTimeout 是 entity.unload 不写 timeout 时等实体锁与在途投影的上限。实际还受 ops.admin_timeout
// 约束（取两者中较早的截止时间）。
const DefaultEntityUnloadTimeout = 5 * time.Second

// UnloadCommand 是 entity.unload 的参数：entity_id（完整 Entity ID）与 player_id（唯一号）二选一。
// player_id 配 kind（EntityKind 数值）得到完整 ID；不写 kind 时在本进程已加载的实体里找唯一号相同的那一个，
// 有多个 kind 都加载着时要求写明 kind。Timeout 是 Go 时长（"5s"），缺省 DefaultEntityUnloadTimeout。
// Reason 只写进日志。
type UnloadCommand struct {
	EntityID int64  `json:"entity_id,omitempty"`
	PlayerID int64  `json:"player_id,omitempty"`
	Kind     uint8  `json:"kind,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RegisterAdminCommands 把实体运维命令注册到 reg（传 app.Lookup(app.ModAdmin) 拿到的实例）。runtime 返回当前的
// DataEngine Runtime（Start 之后才有；之前执行命令明确失败）。
//
//   - entity.unload {entity_id | player_id + kind?, timeout?, reason?}：排空该实体的在途投影后把它从本进程内存卸载，
//     期间写到它的事务以可重试的 dataengine.ErrEntityUnloading 拒绝；下一次访问按正常冷加载从库读回（Runtime.UnloadEntity）。
//     回答 entity_id、kind、resident（命令开始时是否常驻）、unloaded、drained（等到投影完成的在途事务数）。
//     没有常驻时成功、resident=false、什么也不做。实体忙（持有它的事务在 timeout 内没结束、另一次卸载进行中、Remote
//     所有权在迁移或隔离）、投影超时、不是持久化实体都明确失败，实体不受影响。
//     玩家在线、仍有 Sync 订阅时，卸载后框架会立刻为订阅者从库重载（nest.unload_resync）；要让实体真正离开内存，先
//     entity.kick。
//
// 命令经 ops 的 /admin/execute 调用，鉴权与审计由 admin 注册表与 ops 统一完成，这里不另设审批（维护者 2026-10-20 决定）。
func RegisterAdminCommands(reg *admin.Registry, runtime func() *Runtime) error {
	if reg == nil {
		return errors.New("dataengine: admin registry is required")
	}
	if runtime == nil {
		return errors.New("dataengine: runtime source is required")
	}
	return reg.Register(admin.CommandDef{
		Name: AdminCommandEntityUnload,
		Description: "unload one entity from this process's memory after its admitted transactions are projected: " +
			"{entity_id | player_id + kind?, timeout?, reason?}. Writes to it are refused (retryable) meanwhile; the next access cold-loads it. " +
			"Answers resident, unloaded and drained; a busy entity or a projection that does not finish within timeout fails and leaves the entity as it was",
		Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
			payload, err := admin.DecodePayload[UnloadCommand](cmd)
			if err != nil {
				return admin.Result{}, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
			}
			timeout := DefaultEntityUnloadTimeout
			if payload.Timeout != "" {
				if timeout, err = time.ParseDuration(payload.Timeout); err != nil || timeout <= 0 {
					return admin.Result{}, fmt.Errorf("%w: timeout %q is not a positive Go duration", admin.ErrCommandInvalid, payload.Timeout)
				}
			}
			current := runtime()
			if current == nil || !current.Ready() {
				return admin.Result{}, errors.New("dataengine: runtime is not started")
			}
			id, err := resolveUnloadTarget(current.access.Manager(), payload)
			if err != nil {
				return admin.Result{}, err
			}
			if id == 0 {
				return admin.Result{Message: fmt.Sprintf("no entity with unique id %d is loaded in this process", payload.PlayerID),
					Data: map[string]any{"player_id": payload.PlayerID, "resident": false, "unloaded": false}}, nil
			}
			bounded, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			unloaded, err := current.UnloadEntity(bounded, id)
			slog.Info("entity.unload", "entity", id, "resident", unloaded.Resident, "unloaded", unloaded.Unloaded, "drained", unloaded.Drained,
				"reason", payload.Reason, "trace_id", cmd.TraceID, "operator", cmd.Operator, "err", err)
			result := admin.Result{Data: map[string]any{
				"entity_id": id, "kind": uint8(entity.GetEntityKindFromID(id)),
				"resident": unloaded.Resident, "unloaded": unloaded.Unloaded, "drained": unloaded.Drained,
			}}
			if err != nil {
				return result, err
			}
			if !unloaded.Resident {
				result.Message = "the entity is not loaded in this process; nothing to do"
			}
			return result, nil
		},
	})
}

// resolveUnloadTarget 把命令参数换成完整 Entity ID；返回 0 表示 player_id 在本进程没有已加载的实体。
func resolveUnloadTarget(manager *entity.EntityManager, payload UnloadCommand) (int64, error) {
	kind := entity.EntityKind(payload.Kind)
	switch {
	case payload.EntityID != 0 && payload.PlayerID != 0:
		return 0, fmt.Errorf("%w: give entity_id or player_id, not both", admin.ErrCommandInvalid)
	case payload.EntityID != 0:
		id, err := entity.NormalizeFullID(payload.EntityID, kind)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
		}
		return id, nil
	case payload.PlayerID <= 0:
		return 0, fmt.Errorf("%w: entity_id or player_id is required", admin.ErrCommandInvalid)
	case kind != entity.EntityKindNone:
		id, err := entity.BuildEntityID(payload.PlayerID, kind)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
		}
		return id, nil
	}
	// 不写 kind：在本进程已注册的 kind 里找唯一号相同、已加载的实体。
	var loaded []int64
	for value := range 256 {
		candidate := entity.EntityKind(value)
		if _, registered := entity.EntityCategoryOfKind(candidate); !registered || candidate == entity.EntityKindNone {
			continue
		}
		id, err := entity.BuildEntityID(payload.PlayerID, candidate)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
		}
		if manager.Get(id) != nil {
			loaded = append(loaded, id)
		}
	}
	switch len(loaded) {
	case 0:
		return 0, nil
	case 1:
		return loaded[0], nil
	}
	kinds := make([]int, 0, len(loaded))
	for _, id := range loaded {
		kinds = append(kinds, int(entity.GetEntityKindFromID(id)))
	}
	slices.Sort(kinds)
	return 0, fmt.Errorf("%w: unique id %d is loaded as kinds %v; give kind", admin.ErrCommandInvalid, payload.PlayerID, kinds)
}
