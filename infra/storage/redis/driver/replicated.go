package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// EvalReplicated 实现 fredis.ReplicatedEvaler（O-M6-3，docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md）：
//
//  1. 选连接：单机 / Sentinel 用客户端本身；Cluster 按第一个键取该槽位的主节点（MasterForKey），WAIT 只发往它。
//  2. 在一条独占的物理连接上流水线发 EVAL 与 ROLE（一次往返）。脚本与 Eval 一样不经驱动重放；流水线里每条
//     命令都证明没执行时才换连接整条重发（同 EvalBatchDurable）。
//  3. ROLE 报告主节点连着的副本数：为 0 时不发 WAIT（单机开发环境不被 WAIT 卡到超时）；否则同一连接上发
//     WAIT numReplicas timeout。WAIT 从不重放：换了连接的 WAIT 只会立即返回，给出假的确认。
//
// Cluster 上脚本回 MOVED / ASK（拓扑刚变，脚本没执行）或取不到槽位主节点时，改经集群客户端普通发送一次
// （跟随重定向），不 WAIT。其他客户端类型普通发送。
func (c *Client) EvalReplicated(ctx context.Context, script string, keys []string, numReplicas int, timeout time.Duration, args ...any) (fredis.ReplicatedEvalResult, error) {
	plain := func(skipped string) (fredis.ReplicatedEvalResult, error) {
		result, err := c.Eval(ctx, script, keys, args...)
		if err != nil {
			return fredis.ReplicatedEvalResult{}, err
		}
		return fredis.ReplicatedEvalResult{Result: result, Skipped: skipped}, nil
	}
	if numReplicas <= 0 || timeout <= 0 {
		return plain(fredis.ReplicatedSkipDisabled)
	}
	var node *goredis.Client
	switch rdb := c.rdb.(type) {
	case *goredis.Client:
		node = rdb
	case *goredis.ClusterClient:
		if len(keys) == 0 {
			return plain(fredis.ReplicatedSkipUnsupported)
		}
		master, err := rdb.MasterForKey(ctx, keys[0])
		if err != nil {
			return plain(fredis.ReplicatedSkipRedirected)
		}
		node = master
	default:
		return plain(fredis.ReplicatedSkipUnsupported)
	}
	for attempt := 0; ; attempt++ {
		result, redirected, retryable, err := evalReplicatedOnConn(ctx, node, script, keys, numReplicas, timeout, args...)
		if redirected {
			return plain(fredis.ReplicatedSkipRedirected)
		}
		if err == nil || !retryable || attempt >= c.resends {
			return result, err
		}
		if waitErr := waitBeforeResend(ctx, attempt); waitErr != nil {
			return fredis.ReplicatedEvalResult{}, errors.Join(err, waitErr)
		}
	}
}

// evalReplicatedOnConn 在 node 的一条新的独占连接上发一次 EVAL + ROLE，必要时再发 WAIT。
// redirected：脚本回了 MOVED / ASK（没执行）。retryable：流水线每条命令都证明没执行，可以换连接重发。
func evalReplicatedOnConn(ctx context.Context, node *goredis.Client, script string, keys []string, numReplicas int, timeout time.Duration, args ...any) (result fredis.ReplicatedEvalResult, redirected, retryable bool, err error) {
	conn := node.Conn()
	defer func() { _ = conn.Close() }()
	pipe := conn.Pipeline()
	evalCmd := newScriptCmd(ctx, "eval", script, keys, args...)
	roleCmd := goredis.NewCmd(ctx, "role")
	_ = pipe.Process(ctx, noReplay{evalCmd})
	_ = pipe.Process(ctx, noReplay{roleCmd})
	_, execErr := pipe.Exec(ctx)
	if scriptErr := evalCmd.Err(); scriptErr != nil {
		if isRedirect(scriptErr) {
			return fredis.ReplicatedEvalResult{}, true, false, scriptErr
		}
		return fredis.ReplicatedEvalResult{}, false, allNotExecuted([]goredis.Cmder{evalCmd, roleCmd}), scriptErr
	}
	result.Result = evalCmd.Val()
	if execErr != nil && roleCmd.Err() == nil {
		// 脚本与 ROLE 各自成功时 Exec 不会报错；保守起见按 ROLE 失败处理。
		result.WaitErr = execErr
		return result, false, false, nil
	}
	replicas, roleErr := connectedReplicas(roleCmd)
	if roleErr != nil {
		result.WaitErr = roleErr
		return result, false, false, nil
	}
	if replicas == 0 {
		result.Skipped = fredis.ReplicatedSkipNoReplicas
		return result, false, false, nil
	}
	waitCmd := goredis.NewIntCmd(ctx, "wait", numReplicas, timeout.Milliseconds())
	_ = conn.Process(ctx, noReplay{waitCmd})
	if waitErr := waitCmd.Err(); waitErr != nil {
		result.WaitErr = waitErr
		return result, false, false, nil
	}
	result.Waited, result.Replicas = true, waitCmd.Val()
	return result, false, false, nil
}

// connectedReplicas 从 ROLE 的回复里数主节点连着的副本。不是主节点时报错（脚本的写会被 READONLY 拒绝，
// 走到这里说明脚本没有写；没有可等的）。
func connectedReplicas(role *goredis.Cmd) (int, error) {
	reply, err := role.Slice()
	if err != nil {
		return 0, err
	}
	if len(reply) < 3 {
		return 0, fmt.Errorf("redis: invalid ROLE reply length %d", len(reply))
	}
	if name, _ := reply[0].(string); name != "master" {
		return 0, fmt.Errorf("redis: ROLE reports %v, not a master", reply[0])
	}
	replicas, ok := reply[2].([]any)
	if !ok {
		return 0, fmt.Errorf("redis: invalid ROLE replica list %T", reply[2])
	}
	return len(replicas), nil
}

// isRedirect 报告错误是不是 Cluster 的 MOVED / ASK 重定向（服务端没有执行命令）。
func isRedirect(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.HasPrefix(text, "MOVED ") || strings.HasPrefix(text, "ASK ")
}

var _ fredis.ReplicatedEvaler = (*Client)(nil)
