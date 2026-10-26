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

// DurableScriptBatch 实现 fredis.DurableScriptBatcher：一批 EVALSHA 与一条 WAITAOF 在同一条独占连接上流水线发出。
//
// 与 EvalBatchDurable 的区别：一批里可以混合不同脚本；每条脚本的服务端错误回复单独放进结果，不让整批失败（业务结论由
// 脚本返回值表达，错误回复只出现在 NOSCRIPT 或脚本内部错误上）；回 NOSCRIPT 的调用确定没执行，在同一连接上用 EVAL 重发
// 这些调用，再补一条 WAITAOF——它统计这条连接上到此为止的全部写，覆盖第一轮已执行的脚本。
//
// 整批不经驱动重放：连接错误、超时都是结果未知，原样返回。只有每条命令的错误都证明没执行（取连接失败等）时换一条新连接
// 整批重发。
func (c *Client) DurableScriptBatch(ctx context.Context, calls []fredis.ScriptCall, numLocal, numReplicas int, timeout time.Duration) ([]fredis.ScriptResult, int64, int64, error) {
	if len(calls) == 0 {
		return nil, 0, 0, nil
	}
	client, ok := c.rdb.(*goredis.Client)
	if !ok {
		return nil, 0, 0, fmt.Errorf("redis: same-connection WAITAOF is unsupported for %T; use a single-primary endpoint", c.rdb)
	}
	for _, call := range calls {
		if call.SHA == "" || call.Source == "" {
			return nil, 0, 0, errors.New("redis: durable script call needs both SHA and source")
		}
	}
	for attempt := 0; ; attempt++ {
		results, local, replicas, sent, err := durableScriptsOnConn(ctx, client, calls, numLocal, numReplicas, timeout)
		if err == nil || errors.Is(err, fredis.ErrWaitAOFFailed) {
			return results, local, replicas, err
		}
		if attempt >= c.resends || !allNotExecuted(sent) {
			return nil, 0, 0, err
		}
		if waitErr := waitBeforeResend(ctx, attempt); waitErr != nil {
			return nil, 0, 0, errors.Join(err, waitErr)
		}
	}
}

// durableScriptsOnConn 在一条新的独占连接上发一轮；需要时在同一连接上补发 NOSCRIPT 的调用。sent 是第一轮发出的命令，
// 供调用方判断“整批确定没执行”。
func durableScriptsOnConn(ctx context.Context, client *goredis.Client, calls []fredis.ScriptCall, numLocal, numReplicas int, timeout time.Duration) ([]fredis.ScriptResult, int64, int64, []goredis.Cmder, error) {
	conn := client.Conn()
	defer func() { _ = conn.Close() }()
	scripts, wait := sendScripts(ctx, conn, calls, func(call fredis.ScriptCall) (string, string) { return "evalsha", call.SHA }, numLocal, numReplicas, timeout)
	sent := append(commandsAsCmders(scripts), wait)
	if err := transportError(sent); err != nil {
		return nil, 0, 0, sent, err
	}
	results := make([]fredis.ScriptResult, len(calls))
	var missing []int
	for i, command := range scripts {
		results[i] = scriptResult(command)
		if results[i].Err != nil && strings.HasPrefix(results[i].Err.Error(), "NOSCRIPT") {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		// NOSCRIPT 是服务端在执行前拒绝：这些调用没有执行。用原文 EVAL 在同一连接上重发，再补一条 WAITAOF。
		retry := make([]fredis.ScriptCall, len(missing))
		for j, i := range missing {
			retry[j] = calls[i]
		}
		again, againWait := sendScripts(ctx, conn, retry, func(call fredis.ScriptCall) (string, string) { return "eval", call.Source }, numLocal, numReplicas, timeout)
		if err := transportError(append(commandsAsCmders(again), againWait)); err != nil {
			// 第一轮的脚本已执行，第二轮结果未知：整批按结果未知返回，调用方用请求摘要查证。
			return nil, 0, 0, nil, err
		}
		for j, i := range missing {
			results[i] = scriptResult(again[j])
		}
		wait = againWait
	}
	local, replicas, err := waitAOFReply(wait)
	return results, local, replicas, nil, err
}

// sendScripts 在 conn 上流水线发出一组脚本与一条 WAITAOF。脚本与 WAITAOF 都不经驱动重放。
func sendScripts(ctx context.Context, conn *goredis.Conn, calls []fredis.ScriptCall, how func(fredis.ScriptCall) (name, payload string), numLocal, numReplicas int, timeout time.Duration) ([]*goredis.Cmd, *goredis.Cmd) {
	pipe := conn.Pipeline()
	commands := make([]*goredis.Cmd, 0, len(calls))
	for _, call := range calls {
		name, payload := how(call)
		command := newScriptCmd(ctx, name, payload, call.Keys, call.Args...)
		_ = pipe.Process(ctx, noReplay{command})
		commands = append(commands, command)
	}
	wait := goredis.NewCmd(ctx, "WAITAOF", numLocal, numReplicas, timeout.Milliseconds())
	_ = pipe.Process(ctx, noReplay{wait})
	_, _ = pipe.Exec(ctx)
	return commands, wait
}

// transportError 返回这一轮里第一条“回复不可用”的错误：连接、超时、取连接失败，以及服务端在执行前的拒绝（LOADING、
// READONLY 等，IsDefinitelyNotExecuted）。有它就不能按逐条结果裁决：每条都确定没执行时调用方整批重发，否则整批结果未知。
// 其余服务端错误回复（NOSCRIPT、WRONGTYPE、脚本内部错误、WAITAOF 拒绝）属于单条结果，连接本身正常。
func transportError(commands []goredis.Cmder) error {
	for _, command := range commands {
		err := command.Err()
		if err == nil || errors.Is(err, goredis.Nil) {
			continue
		}
		var reply goredis.Error
		if IsDefinitelyNotExecuted(err) || !errors.As(err, &reply) {
			return err
		}
	}
	return nil
}

func scriptResult(command *goredis.Cmd) fredis.ScriptResult {
	value, err := command.Result()
	if errors.Is(err, goredis.Nil) {
		return fredis.ScriptResult{}
	}
	return fredis.ScriptResult{Value: value, Err: err}
}

// waitAOFReply 解析 WAITAOF 回复；服务端拒绝 WAITAOF 时返回 ErrWaitAOFFailed（脚本已执行）。
func waitAOFReply(wait *goredis.Cmd) (int64, int64, error) {
	reply, err := wait.Slice()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", fredis.ErrWaitAOFFailed, err)
	}
	if len(reply) != 2 {
		return 0, 0, fmt.Errorf("%w: invalid WAITAOF reply length %d", fredis.ErrWaitAOFFailed, len(reply))
	}
	local, err := redisInteger(reply[0])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid WAITAOF local reply: %v", fredis.ErrWaitAOFFailed, err)
	}
	replicas, err := redisInteger(reply[1])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid WAITAOF replica reply: %v", fredis.ErrWaitAOFFailed, err)
	}
	return local, replicas, nil
}

var _ fredis.DurableScriptBatcher = (*Client)(nil)
