package configdata

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// 配置表的运维命令。
//
// 重载配置表是运维动作，和 hotcode.list / bus.dlq.* 一样由框架提供：wiring/configdata 的 Mod 在
// Provide 时把它们挂到 App 的 admin 注册表（经 ops 的 /admin/execute 调用，同一套 token 与审计），
// 业务工程不用再手写一段“查 Store、调 ReloadWithReason”的 GM 命令。
const (
	AdminCommandReload   = "configdata.reload"
	AdminCommandRollback = "configdata.rollback"
	AdminCommandStatus   = "configdata.status"
)

// ReloadCommand 是 configdata.reload / configdata.rollback 的参数。Reason 写进日志（不进指标标签），
// 空时为 "admin"。DryRun 只对 reload 有效：构建并校验候选数据、报告变化，但不发布。
type ReloadCommand struct {
	Reason string `json:"reason,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// noChangeMessage 是内容完全没变时的回答：重载照常发布了新版本号，但数据与上一代相同。最常见的原因是
// 只改了 CSV、没有重新生成 JSON。
func noChangeMessage(dir string) string {
	return fmt.Sprintf("no table changed: the content equals the previous generation. Reload reads the JSON under %q; if you edited a CSV, run `roost generate` first", dir)
}

// RegisterAdminCommands 把配置表的重载、回滚与状态命令注册到 reg（传 app.Lookup(app.ModAdmin) 拿到的
// 实例；包级默认注册表是另一个实例，装配路径不读它）。
//
//   - configdata.reload {reason?, dry_run?}：重新读取 config_data.dir 下的数据并发布。回答 version
//     （新版本号）、previous（发布前在服务的版本）、hash、changed（是否有成员变化）与 changes（新增 / 删除 /
//     变化的表及行数）。内容完全没变时仍按 Store 协议发布新版本号，但 changed=false，并在 message 里说明；
//     违反表规则时整次拒绝、旧快照继续服务，错误原样返回。
//   - configdata.rollback {reason?}：回到上一代（只保留一级），回答同样的字段。
//   - configdata.status {}：当前版本、hash、加载时间与各表行数。
func RegisterAdminCommands(reg *admin.Registry, store *Store) error {
	if reg == nil {
		return errors.New("configdata: admin registry is required")
	}
	if store == nil {
		return errors.New("configdata: store is required")
	}
	return errors.Join(
		reg.Register(admin.CommandDef{
			Name: AdminCommandReload,
			Description: "reload the config tables from config_data.dir: {reason?, dry_run?}. " +
				"Answers the new version and which tables changed; a reload whose content equals the previous generation says so (changed=false). " +
				"Reload reads JSON only: after editing a CSV, run `roost generate` first. A table that breaks its rules fails the whole reload and the live snapshot stays.",
			Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
				payload, err := admin.DecodePayload[ReloadCommand](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				reason := commandReason(payload.Reason)
				if payload.DryRun {
					live := store.Current()
					candidate, err := store.DryRun(ctx, reason)
					if err != nil {
						return admin.Result{}, err
					}
					result := reloadResultData(store, ReloadResult{Snapshot: candidate, Previous: snapshotVersion(live), Changes: Compare(live, candidate)}, reason)
					result.Data["dry_run"] = true
					return result, nil
				}
				applied, err := store.ReloadWithChanges(ctx, reason)
				if err != nil {
					return admin.Result{}, err
				}
				return reloadResultData(store, applied, reason), nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandRollback,
			Description: "roll the config tables back to the previous generation (one level): {reason?}. Answers the version gone back to and which tables changed",
			Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
				payload, err := admin.DecodePayload[ReloadCommand](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				reason := commandReason(payload.Reason)
				applied, err := store.RollbackWithChanges(ctx, reason)
				if err != nil {
					return admin.Result{}, err
				}
				return reloadResultData(store, applied, reason), nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandStatus,
			Description: "the config generation serving now: {} — version, hash, loaded_at, data dir and the row count of every table",
			Handler: func(context.Context, admin.Command) (admin.Result, error) {
				snap := store.Current()
				if snap == nil {
					return admin.Result{Message: "no config generation loaded yet", Data: map[string]any{"version": uint64(0), "dir": store.Dir()}}, nil
				}
				tables := make(map[string]int, len(snap.members))
				for key, member := range snap.members {
					if key.kind == "table" {
						tables[string(key.name)] = max(member.rows, 0)
					}
				}
				return admin.Result{Data: map[string]any{
					"version":   snap.Version,
					"hash":      snap.Hash,
					"loaded_at": snap.LoadedAt.UTC().Format("2006-01-02T15:04:05Z"),
					"dir":       store.Dir(),
					"tables":    tables,
				}}, nil
			},
		}),
	)
}

func commandReason(reason string) string {
	if reason == "" {
		return "admin"
	}
	return reason
}

func reloadResultData(store *Store, applied ReloadResult, reason string) admin.Result {
	result := admin.Result{Data: map[string]any{
		"reason":   reason,
		"version":  applied.Snapshot.Version,
		"previous": applied.Previous,
		"hash":     applied.Snapshot.Hash,
		"changed":  !applied.Changes.None(),
		"changes":  applied.Changes,
	}}
	if applied.Changes.None() {
		result.Message = noChangeMessage(store.Dir())
	}
	return result
}
