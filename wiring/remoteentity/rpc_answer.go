package remoteentity

import (
	"context"
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
)

// RPCWriteAnswer 把实体服务里一次 Remote 实体写（RPC handler 在 I/O 段调生成的 Sync_* Sender）的错误转成按槽位
// 路由 RPC 的答复（联盟服务方案 §2.3、§2.7），由 handler 写进响应：code, reason := servicerpc.Error(answer)，
// 响应的 RedirectSID 字段取 servicerpc.RedirectSID(answer)。
//
//   - nil、业务错误：原样返回（成功 / 业务拒绝）。
//   - 结果未知（entity.ErrRemotePersistenceIndeterminate、ErrRemoteCommitTimeout、ctx 到期）：原样返回，调用方按
//     CodeInternal / 超时归为“结果未知”，用同一 RequestID 查结果。先于下面各条判断：结果未知的错误可能同时包着 fence。
//   - 确定没执行、可重试：重载窗口（ErrRemoteEntityReloading）、所有权转移中（ErrRemoteOwnerTransition）、写额度满
//     （ErrRemoteOverloaded）、准入时版本冲突（ErrRemoteVersionConflict）→ servicerpc.Retryable。
//   - 本实例不是 owner（ErrRemoteFenced）：读一次权威的所有权记录，owner 是别的 sid 就答复 servicerpc.Redirect(owner)，
//     读不到或 owner 仍是本实例就答复 Retryable。读所有权要 I/O，所以只能在 I/O 段（RPC handler）里调。
//
// 槽位表只是路由提示，真正的 owner 以这里读到的持久记录为准。
func RPCWriteAnswer(ctx context.Context, owners entity.RemoteOwnershipManager, localSID int32, id int64, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, entity.ErrRemotePersistenceIndeterminate), errors.Is(err, entity.ErrRemoteCommitTimeout),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, entity.ErrRemoteEntityReloading), errors.Is(err, entity.ErrRemoteOwnerTransition),
		errors.Is(err, entity.ErrRemoteOverloaded), errors.Is(err, entity.ErrRemoteVersionConflict):
		return servicerpc.Retryable(err)
	case errors.Is(err, entity.ErrRemoteFenced):
		if owners == nil {
			return servicerpc.Retryable(err)
		}
		lease, found, lookupErr := owners.GetRemoteOwnership(ctx, id)
		if lookupErr != nil || !found || lease.OwnerSid == localSID {
			return servicerpc.Retryable(errors.Join(err, lookupErr))
		}
		return servicerpc.Redirect(lease.OwnerSid, err)
	default:
		return err
	}
}
