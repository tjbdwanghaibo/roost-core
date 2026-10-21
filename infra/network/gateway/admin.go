package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// AdminCommandEntityKick 是踢下线的 admin 命令名（D16）。
const AdminCommandEntityKick = "entity.kick"

// ErrSessionKicked 是 entity.kick 交给 PlayerRuntime.CloseSessions 的关闭原因（带 reason 时包裹它），
// 用 errors.Is 区分“运维踢下线”与网络断开。会话关闭事件（TCPSessionClosed）本身不带原因。
var ErrSessionKicked = errors.New("gateway: session closed by an operator (entity.kick)")

// KickCommand 是 entity.kick 的参数。Reason 只写进日志（不进指标标签、不发给客户端）。
type KickCommand struct {
	PlayerID int64  `json:"player_id"`
	Reason   string `json:"reason,omitempty"`
}

// RegisterAdminCommands 把玩家接入层的运维命令注册到 reg（传 app.Lookup(app.ModAdmin) 拿到的实例）：
//
//   - entity.kick {player_id, reason?}：关闭该玩家在本进程的全部会话，回答 closed（本次关闭的会话数）。
//     players 是本进程唯一的玩家接入（嵌入 TCP 的 TCPRuntime 或独立 Gate 部署下的 GameIngress，两者都实现
//     PlayerRuntime.CloseSessions），关闭与网络错误走同一条路径：会话关闭事件照常送达每个订阅者，场景、聊天在线、
//     自身视图等清理与断线时完全一样。不改实体状态、不卸载实体、不封禁：玩家可以立刻重新登录。
//     玩家没有会话时成功、closed=0（幂等）。只作用于本进程：独立 Gate 部署下 Game 进程关闭的是它持有的绑定，
//     Gate 随即断开对应客户端连接。
//
// 命令经 ops 的 /admin/execute 调用，鉴权（ops.admin_token）与审计（admin command started / finished 日志、
// admin.execute.total 指标）都由 admin 注册表与 ops 统一完成，这里不另设审批（维护者 2026-10-20 决定）。
func RegisterAdminCommands(reg *admin.Registry, players PlayerRuntime) error {
	if reg == nil {
		return errors.New("gateway: admin registry is required")
	}
	if players == nil {
		return errors.New("gateway: player runtime is required")
	}
	return reg.Register(admin.CommandDef{
		Name: AdminCommandEntityKick,
		Description: "close every session of one player on this process: {player_id, reason?}. Answers closed (sessions this call closed); " +
			"0 when the player has none here. The entity state is not touched and the player may log in again",
		Handler: func(_ context.Context, cmd admin.Command) (admin.Result, error) {
			payload, err := admin.DecodePayload[KickCommand](cmd)
			if err != nil {
				return admin.Result{}, fmt.Errorf("%w: %v", admin.ErrCommandInvalid, err)
			}
			if payload.PlayerID <= 0 {
				return admin.Result{}, fmt.Errorf("%w: player_id is required", admin.ErrCommandInvalid)
			}
			cause := ErrSessionKicked
			if payload.Reason != "" {
				cause = fmt.Errorf("%w: %s", ErrSessionKicked, payload.Reason)
			}
			closed := players.CloseSessions(payload.PlayerID, cause)
			slog.Info("entity.kick: closed the player's sessions", "player_id", payload.PlayerID, "closed", closed,
				"reason", payload.Reason, "trace_id", cmd.TraceID, "operator", cmd.Operator)
			result := admin.Result{Data: map[string]any{"player_id": payload.PlayerID, "closed": closed}}
			if closed == 0 {
				result.Message = "the player has no session on this process"
			}
			return result, nil
		},
	})
}
