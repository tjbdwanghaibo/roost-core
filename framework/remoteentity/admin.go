package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// AdminCommandEmptyClaims 是列出 / 删除本进程空认领的 admin 命令名（D17）。
const AdminCommandEmptyClaims = "remote_entity.empty_claims"

// DefaultEmptyClaimPage 是 remote_entity.empty_claims 不写 limit 时一页处理的候选数。
const DefaultEmptyClaimPage = 100

// EmptyClaimsCommand 是 remote_entity.empty_claims 的参数。
//
//   - Kinds：EntityKind 数值；不写时取本进程注册的全部 Remote 托管 kind（归属别的服务的除外）。
//   - After / Limit：分页，按 ID 升序从 after 之后取至多 limit 个候选（缺省 DefaultEmptyClaimPage，上限
//     OwnedListPageLimit）；回答里的 next 是下一页的 after，0 表示到底。
//   - Delete：false（缺省）只列候选，不改任何东西；true 对这一页的每个候选执行 Manager.DeleteEmptyClaim。
//   - Reason：只写进日志。
type EmptyClaimsCommand struct {
	Kinds  []uint8 `json:"kinds,omitempty"`
	After  int64   `json:"after,omitempty"`
	Limit  int     `json:"limit,omitempty"`
	Delete bool    `json:"delete,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// RegisterAdminCommands 把 Remote 的运维命令注册到 reg（传 app.Lookup(app.ModAdmin) 拿到的实例）。manager 返回本进程的
// 写 Manager（Assemble 之后才有；没有时执行命令明确失败）。
//
//   - remote_entity.empty_claims {kinds?, after?, limit?, delete?, reason?}（D17）：列出本 sid 独占、权威记录看起来是空认领
//     的实体（候选，Manager.ListEmptyClaims），delete=true 时逐个执行 Manager.DeleteEmptyClaim（写门下复核、带所有权
//     代际守护的比较删除）。回答 candidates（这一页的候选 ID）、next；删除时另答 deleted / not_empty / not_owned /
//     changed / failed 的个数与 deleted_ids。单个候选失败只计数、记日志，继续下一个；列不出候选时命令失败。
//     只作用于收到命令的进程：别的 sid 持有的空认领发给那个 sid。
//
// 命令经 ops 的 /admin/execute 调用，鉴权与审计由 admin 注册表与 ops 统一完成（与 entity.unload 相同）。
func RegisterAdminCommands(reg *admin.Registry, manager func() *Manager) error {
	if reg == nil {
		return errors.New("remote_entity: admin registry is required")
	}
	if manager == nil {
		return errors.New("remote_entity: manager source is required")
	}
	return reg.Register(admin.CommandDef{
		Name: AdminCommandEmptyClaims,
		Description: "list this process's empty claims (Remote entities it owns exclusively whose ownership record has no commit yet): " +
			"{kinds?, after?, limit?, delete?, reason?}. Answers candidates and next. delete=true deletes each candidate whose ownership " +
			"generation is unchanged and still empty (compare-and-delete under the entity write gate) and answers deleted / not_empty / not_owned / changed / failed",
		Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
			payload, err := admin.DecodePayload[EmptyClaimsCommand](cmd)
			if err != nil {
				return admin.Result{}, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
			}
			if payload.Limit < 0 || payload.After < 0 {
				return admin.Result{}, fmt.Errorf("%w: after and limit must not be negative", admin.ErrCommandInvalid)
			}
			limit := payload.Limit
			if limit == 0 {
				limit = DefaultEmptyClaimPage
			}
			mgr := manager()
			if mgr == nil {
				return admin.Result{}, errors.New("remote_entity: manager is not assembled")
			}
			kinds, err := mgr.emptyClaimKinds(payload.Kinds)
			if err != nil {
				return admin.Result{}, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
			}
			candidates, next, err := mgr.ListEmptyClaims(ctx, kinds, payload.After, limit)
			if err != nil {
				return admin.Result{}, err
			}
			data := map[string]any{"candidates": candidates, "next": next}
			if !payload.Delete {
				return admin.Result{Message: fmt.Sprintf("%d empty-claim candidates (not deleted; pass delete=true to delete)", len(candidates)), Data: data}, nil
			}
			counts := map[string]int{}
			deletedIDs := make([]int64, 0, len(candidates))
			for _, id := range candidates {
				outcome, err := mgr.DeleteEmptyClaim(ctx, id)
				if err != nil {
					counts["failed"]++
					slog.Warn("remote_entity.empty_claims: delete failed; the claim is left as it was or as the authority now says",
						"entity", id, "err", err, "trace_id", cmd.TraceID)
					continue
				}
				counts[outcome.String()]++
				if outcome == EmptyClaimDeleted {
					deletedIDs = append(deletedIDs, id)
				}
			}
			for _, key := range []string{"deleted", "not_empty", "not_owned", "changed", "failed"} {
				data[key] = counts[key]
			}
			data["deleted_ids"] = deletedIDs
			slog.Info("remote_entity.empty_claims", "sid", mgr.localSid, "candidates", len(candidates), "deleted", counts["deleted"],
				"not_empty", counts["not_empty"], "not_owned", counts["not_owned"], "changed", counts["changed"], "failed", counts["failed"],
				"next", next, "reason", payload.Reason, "trace_id", cmd.TraceID, "operator", cmd.Operator)
			return admin.Result{Data: data}, nil
		},
	})
}

// emptyClaimKinds 把命令里的 kind 换成要查的列表：不写时取全部已注册的 Remote 托管 kind（归属别的服务的除外）；写了的
// 必须是已注册的 Remote 托管 kind，归属别的服务的报错。
func (m *Manager) emptyClaimKinds(raw []uint8) ([]entity.EntityKind, error) {
	if len(raw) == 0 {
		var kinds []entity.EntityKind
		for value := 1; value < 256; value++ {
			kind := entity.EntityKind(value)
			if entity.IsEntityKindRemoteManaged(kind) {
				if _, foreign := m.foreignKinds[kind]; !foreign {
					kinds = append(kinds, kind)
				}
			}
		}
		return kinds, nil
	}
	kinds := make([]entity.EntityKind, 0, len(raw))
	for _, value := range raw {
		kind := entity.EntityKind(value)
		if !entity.IsEntityKindRemoteManaged(kind) {
			return nil, fmt.Errorf("kind %d is not a registered Remote-managed kind", value)
		}
		if _, foreign := m.foreignKinds[kind]; foreign {
			return nil, fmt.Errorf("kind %d is owned by another service (WithForeignKinds); send the command to that service", value)
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}
