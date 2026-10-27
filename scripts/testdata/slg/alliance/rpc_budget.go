package alliance

import (
	"context"
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

var errReceiverBudget = errors.New("alliance: receiver budget must be positive")

// receiverBudget 给业务自己注册的普通 Bus RPC（Bus.HandleRpc，例如原服接收地图战报、接收跨服义务）加接收端
// 处理期限：handle 收到的 ctx 带本端预算 budget。
//
// 框架契约（bus.go dispatchRpc）：普通 RPC 的 RpcContext.Context() 是接收端 Bus 的基础 context，只在 Bus 停止时
// 取消；调用方 CallTo 的 deadline 不随请求传过来，调用方超时只是停止等待。接收端不自己限时，按 ctx 等待的回调会
// 一直跑到停机（JetStream RPC 不同，它按调用方截止建立处理期限）。所以接收业务要给整次处理一个自己的预算。
//
// 期限内没完成、或回调成功返回时预算已过，都返回错误、不回复成功：调用方多半已放弃，迟到的成功不能当作确认，
// 由调用方按结果未知用原身份重试，靠接收方的永久回执去重。预算不能强杀忽略取消的回调，停机仍要等它真实排空。
// 学习分支 fix/core-game-server-review 的 b6f9b524（RoutedReportReceiver.Timeout）是同一修正。
func receiverBudget(budget time.Duration, handle func(context.Context, *bus.RpcContext) (any, error)) (bus.RpcHandlerFunc, error) {
	if budget <= 0 || handle == nil {
		return nil, errReceiverBudget
	}
	return func(c *bus.RpcContext) (any, error) {
		ctx, cancel := context.WithTimeout(c.Context(), budget)
		defer cancel()
		out, err := handle(ctx, c)
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	}, nil
}
