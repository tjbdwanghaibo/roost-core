package driver

import (
	"context"
	"errors"
	"io"
	"net"

	goredis "github.com/redis/go-redis/v9"
)

// 节点断连时主动刷新槽位缓存。当前客户端只收到连接错误时可能沿用旧主地址到
// 周期刷新（默认 60 秒），即使 Redis 已完成选主，业务仍无法恢复。
// 这里只请求合并后的异步拓扑刷新，原错误原样返回；不重放结果不确定的写命令。
type clusterRecoveryHook struct{ client *goredis.ClusterClient }

func (h clusterRecoveryHook) refreshAfter(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		h.client.ReloadState(context.Background())
	}
}
func (h clusterRecoveryHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h clusterRecoveryHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		h.refreshAfter(err)
		return err
	}
}
func (h clusterRecoveryHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		err := next(ctx, cmds)
		h.refreshAfter(err)
		return err
	}
}
