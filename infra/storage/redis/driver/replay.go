package driver

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// 本文件是驱动的“重放策略”：哪些命令交给 go-redis 自动重试，哪些只发一次（A2 决定，
// 行为契约表见 redis/driver/README.md）。
//
// go-redis 的 MaxRetries 在连接 EOF / 读超时之后会把同一条命令换连接再发：对读命令无害；
// 对写命令，“发出去了、回复丢了”时服务端可能已经执行，再发就是第二次执行——INCR 加两次、
// RPUSH 追加两次、SETNX 把自己刚拿到的键报成“已被占用”、DEL / HSET 的计数报小、整条
// pipeline 重放（RR-20261005-NC-100 是脚本的同一问题）。结果未知只能由调用方按自己的语义裁决
// （请求 ID、版本 / CAS、值守卫令牌），所以写命令一律带 NoRetry 交给驱动，驱动在任何错误上都
// 不重发；这里只在错误能证明命令根本没有执行时才重发（IsDefinitelyNotExecuted）。

// IsDefinitelyNotExecuted 判断一次 Redis 调用的错误是否证明命令没有在服务端执行。
//
// true 只给这几类（按 go-redis v9.22.0 源码核对，error.go shouldRetry / internal/pool）：
//   - 取连接失败，请求还没写出：dial 失败（*net.OpError Op=="dial"，含拨号超时）、
//     ErrPoolTimeout、ErrPoolExhausted、ErrClosed（客户端已关闭）。池里的坏连接在取出时由
//     go-redis 的健康检查丢弃并换新连接，不会以错误出现；换新连接时的拨号失败落在上一条。
//   - 服务端在执行之前拒绝：LOADING、MASTERDOWN、TRYAGAIN、CLUSTERDOWN、max number of clients、
//     READONLY、NOREPLICAS（判定沿用 go-redis 的 IsXxxError：类型化错误，或错误文本以该前缀开头）。脚本里的写命令遇到 READONLY / NOREPLICAS 时也是第一条写就被拒、
//     之前没有写入；框架的脚本不会自己返回带这些前缀的错误回复。
//
// 其余一律是“结果未知”：EOF、连接重置、读写超时、调用方 ctx 取消或到期（可能落在写出之后），
// 以及连接握手阶段的失败（值上与写出之后的失败无法区分，保守归为未知）。
// pipeline 只有每条命令的错误都满足本函数时才是“整条未执行”。
func IsDefinitelyNotExecuted(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	if errors.Is(err, goredis.ErrPoolTimeout) || errors.Is(err, goredis.ErrPoolExhausted) || errors.Is(err, goredis.ErrClosed) {
		return true
	}
	return goredis.IsLoadingError(err) ||
		goredis.IsMasterDownError(err) ||
		goredis.IsTryAgainError(err) ||
		goredis.IsClusterDownError(err) ||
		goredis.IsMaxClientsError(err) ||
		goredis.IsReadOnlyError(err) ||
		goredis.IsNoReplicasError(err)
}

// shouldResend：确定没执行、且再发一次有意义（客户端已关闭时重发不会成功）。
func shouldResend(err error) bool {
	return IsDefinitelyNotExecuted(err) && !errors.Is(err, goredis.ErrClosed)
}

// noReplay 给任意 go-redis 命令加上 NoRetry：驱动的单命令重试、Cluster 的换节点重试、
// 含它的 pipeline 整体重试都会跳过（redis.go processWithRetry / generalProcessPipeline、
// osscluster.go process）。Cluster 的 MOVED / ASK 重定向在 NoRetry 检查之前，照常跟随。
type noReplay struct{ goredis.Cmder }

func (noReplay) NoRetry() bool { return true }

// Clone 保留标记；路由层复制命令时不能退回可重放的原命令。
func (c noReplay) Clone() goredis.Cmder { return noReplay{c.Cmder.Clone()} }

// resendsFor 把配置的 MaxRetries 换成“确定未执行时的重发次数”，取值规则与 go-redis 相同：
// -1 不重发，0 取缺省 3。
func resendsFor(maxRetries int) int {
	switch {
	case maxRetries < 0:
		return 0
	case maxRetries == 0:
		return 3
	default:
		return maxRetries
	}
}

// 重发退避与 go-redis v9.22.0 的缺省（MinRetryBackoff 10ms、MaxRetryBackoff 1s）和公式
// （internal.RetryBackoff：min + rand[0, min<<attempt)，封顶 max）相同。
const (
	resendBackoffMin = 10 * time.Millisecond
	resendBackoffMax = time.Second
)

// waitBeforeResend 在第 attempt 次重发前退避；ctx 先结束时返回 ctx 的错误。
func waitBeforeResend(ctx context.Context, attempt int) error {
	d := resendBackoffMax
	if attempt < 16 {
		d = min(resendBackoffMin+time.Duration(rand.Int64N(int64(resendBackoffMin<<attempt))), resendBackoffMax)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sendOnce 发一条不可重放的命令：驱动不重发；错误证明它没执行时由这里重发，至多 resends 次。
// 退避中 ctx 结束时返回“上一次错误 + ctx 错误”（仍是确定未执行，errors.Is ctx 错误成立）。
// 返回的错误同时写回 cmd。
func sendOnce(ctx context.Context, rdb goredis.UniversalClient, resends int, cmd goredis.Cmder) error {
	wrapped := noReplay{cmd}
	for attempt := 0; ; attempt++ {
		err := rdb.Process(ctx, wrapped)
		if err == nil || attempt >= resends || !shouldResend(err) {
			return err
		}
		if waitErr := waitBeforeResend(ctx, attempt); waitErr != nil {
			err = errors.Join(err, waitErr)
			cmd.SetErr(err)
			return err
		}
	}
}

// buildWrite 用 go-redis 的类型化方法组装一条命令（参数展开、过期格式与直接调用完全一致），
// 只入队到一个用完即弃的 Pipeliner、不经它发送。
func buildWrite[C goredis.Cmder](rdb goredis.UniversalClient, build func(goredis.Pipeliner) C) C {
	return build(rdb.Pipeline())
}

// write 组装并按 sendOnce 发出一条写命令，返回带结果的原命令。
func write[C goredis.Cmder](ctx context.Context, c *Client, build func(goredis.Pipeliner) C) C {
	cmd := buildWrite(c.rdb, build)
	_ = sendOnce(ctx, c.rdb, c.resends, cmd)
	return cmd
}

// allNotExecuted：pipeline 里每条命令都带着“确定未执行、值得重发”的错误。
func allNotExecuted(cmds []goredis.Cmder) bool {
	if len(cmds) == 0 {
		return false
	}
	for _, cmd := range cmds {
		if !shouldResend(cmd.Err()) {
			return false
		}
	}
	return true
}
