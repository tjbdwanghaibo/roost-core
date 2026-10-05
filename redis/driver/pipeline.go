package driver

import (
	"context"
	"errors"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// pipeline 实现 fredis.IPipeline。读命令照常入队；写命令带 NoRetry 入队，于是整条 pipeline
// 不经驱动重放（回复丢失时前面的写可能已经执行）。Exec 只在每条命令的错误都证明没执行时整条重发（A2）。
type pipeline struct {
	rdb       goredis.UniversalClient
	pipe      goredis.Pipeliner
	resends   int
	hasWrites bool
	// track futures for result assignment after Exec
	bytesFutures     []*pipelineBytesCmd
	stringMapFutures []*pipelineStringMapCmd
	int64Futures     []*pipelineInt64Cmd
}

type pipelineBytesCmd struct {
	cmd    *goredis.StringCmd
	future *fredis.FutureBytes
}

type pipelineInt64Cmd struct {
	cmd    *goredis.IntCmd
	future *fredis.FutureInt64
}

type pipelineStringMapCmd struct {
	cmd    *goredis.MapStringStringCmd
	future *fredis.FutureStringMap
}

func newPipeline(rdb goredis.UniversalClient, resends int) *pipeline {
	return &pipeline{rdb: rdb, pipe: rdb.Pipeline(), resends: resends}
}

// queueWrite 组装一条写命令并以不可重放的形式入队。
func queueWrite[C goredis.Cmder](ctx context.Context, p *pipeline, build func(goredis.Pipeliner) C) C {
	cmd := buildWrite(p.rdb, build)
	p.hasWrites = true
	_ = p.pipe.Process(ctx, noReplay{cmd})
	return cmd
}

func (p *pipeline) Get(ctx context.Context, key string) *fredis.FutureBytes {
	cmd := p.pipe.Get(ctx, key)
	f := &fredis.FutureBytes{}
	p.bytesFutures = append(p.bytesFutures, &pipelineBytesCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Set(ctx context.Context, key string, value any, expiration time.Duration) {
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.StatusCmd { return b.Set(ctx, key, value, expiration) })
}

func (p *pipeline) Del(ctx context.Context, keys ...string) {
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.IntCmd { return b.Del(ctx, keys...) })
}

func (p *pipeline) HSet(ctx context.Context, key string, values ...any) {
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.IntCmd { return b.HSet(ctx, key, values...) })
}

func (p *pipeline) HGet(ctx context.Context, key, field string) *fredis.FutureBytes {
	cmd := p.pipe.HGet(ctx, key, field)
	f := &fredis.FutureBytes{}
	p.bytesFutures = append(p.bytesFutures, &pipelineBytesCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) HGetAll(ctx context.Context, key string) *fredis.FutureStringMap {
	cmd := p.pipe.HGetAll(ctx, key)
	f := &fredis.FutureStringMap{}
	p.stringMapFutures = append(p.stringMapFutures, &pipelineStringMapCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Incr(ctx context.Context, key string) *fredis.FutureInt64 {
	cmd := queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.IntCmd { return b.Incr(ctx, key) })
	f := &fredis.FutureInt64{}
	p.int64Futures = append(p.int64Futures, &pipelineInt64Cmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Expire(ctx context.Context, key string, expiration time.Duration) {
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.BoolCmd { return b.Expire(ctx, key, expiration) })
}

func (p *pipeline) ZAdd(ctx context.Context, key string, members ...fredis.Z) {
	zs := make([]goredis.Z, len(members))
	for i, m := range members {
		zs[i] = goredis.Z{Score: m.Score, Member: m.Member}
	}
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.IntCmd { return b.ZAdd(ctx, key, zs...) })
}

func (p *pipeline) RPush(ctx context.Context, key string, values ...any) {
	queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.IntCmd { return b.RPush(ctx, key, values...) })
}

func (p *pipeline) LPop(ctx context.Context, key string) *fredis.FutureBytes {
	cmd := queueWrite(ctx, p, func(b goredis.Pipeliner) *goredis.StringCmd { return b.LPop(ctx, key) })
	f := &fredis.FutureBytes{}
	p.bytesFutures = append(p.bytesFutures, &pipelineBytesCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Exec(ctx context.Context) error {
	defer func() {
		p.bytesFutures = nil
		p.stringMapFutures = nil
		p.int64Futures = nil
		p.hasWrites = false
	}()
	commands, err := p.pipe.Exec(ctx)
	// 只读的 pipeline 已由驱动按自己的规则重试；含写的整条只在确定未执行时重发。
	for attempt := 0; p.hasWrites && err != nil && attempt < p.resends && allNotExecuted(commands); attempt++ {
		if waitErr := waitBeforeResend(ctx, attempt); waitErr != nil {
			err = errors.Join(err, waitErr)
			break
		}
		for _, command := range commands {
			command.SetErr(nil)
			_ = p.pipe.Process(ctx, command)
		}
		commands, err = p.pipe.Exec(ctx)
	}
	// Assign results to futures
	for _, bc := range p.bytesFutures {
		val, cmdErr := bc.cmd.Bytes()
		if cmdErr == goredis.Nil {
			bc.future.SetResult(nil, fredis.ErrNil)
		} else {
			bc.future.SetResult(val, cmdErr)
		}
	}
	for _, ic := range p.int64Futures {
		val, cmdErr := ic.cmd.Result()
		ic.future.SetResult(val, cmdErr)
	}
	for _, mc := range p.stringMapFutures {
		val, cmdErr := mc.cmd.Result()
		mc.future.SetResult(val, cmdErr)
	}
	// Preserve transport/aggregate errors after filling every future. Redis
	// Nil may be the first command error while a later write failed; writes
	// without futures still need their error reported by Exec.
	if err != nil && err != goredis.Nil {
		return err
	}
	for _, command := range commands {
		if commandErr := command.Err(); commandErr != nil && commandErr != goredis.Nil {
			return commandErr
		}
	}
	return nil
}

func (p *pipeline) Discard() {
	p.pipe.Discard()
	p.bytesFutures = nil
	p.stringMapFutures = nil
	p.int64Futures = nil
	p.hasWrites = false
}

var _ fredis.IPipeline = (*pipeline)(nil)
