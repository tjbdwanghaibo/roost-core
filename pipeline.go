package redis

import (
	"context"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type pipeline struct {
	pipe goredis.Pipeliner
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

func newPipeline(pipe goredis.Pipeliner) *pipeline {
	return &pipeline{pipe: pipe}
}

func (p *pipeline) Get(ctx context.Context, key string) *fredis.FutureBytes {
	cmd := p.pipe.Get(ctx, key)
	f := &fredis.FutureBytes{}
	p.bytesFutures = append(p.bytesFutures, &pipelineBytesCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Set(ctx context.Context, key string, value any, expiration time.Duration) {
	p.pipe.Set(ctx, key, value, expiration)
}

func (p *pipeline) Del(ctx context.Context, keys ...string) {
	p.pipe.Del(ctx, keys...)
}

func (p *pipeline) HSet(ctx context.Context, key string, values ...any) {
	p.pipe.HSet(ctx, key, values...)
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
	cmd := p.pipe.Incr(ctx, key)
	f := &fredis.FutureInt64{}
	p.int64Futures = append(p.int64Futures, &pipelineInt64Cmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Expire(ctx context.Context, key string, expiration time.Duration) {
	p.pipe.Expire(ctx, key, expiration)
}

func (p *pipeline) ZAdd(ctx context.Context, key string, members ...fredis.Z) {
	zs := make([]goredis.Z, len(members))
	for i, m := range members {
		zs[i] = goredis.Z{Score: m.Score, Member: m.Member}
	}
	p.pipe.ZAdd(ctx, key, zs...)
}

func (p *pipeline) RPush(ctx context.Context, key string, values ...any) {
	p.pipe.RPush(ctx, key, values...)
}

func (p *pipeline) LPop(ctx context.Context, key string) *fredis.FutureBytes {
	cmd := p.pipe.LPop(ctx, key)
	f := &fredis.FutureBytes{}
	p.bytesFutures = append(p.bytesFutures, &pipelineBytesCmd{cmd: cmd, future: f})
	return f
}

func (p *pipeline) Exec(ctx context.Context) error {
	defer func() {
		p.bytesFutures = nil
		p.stringMapFutures = nil
		p.int64Futures = nil
	}()
	_, err := p.pipe.Exec(ctx)
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
	// Return pipeline-level error (ignoring individual Nil errors)
	if err != nil && err != goredis.Nil {
		return err
	}
	return nil
}

func (p *pipeline) Discard() {
	p.pipe.Discard()
	p.bytesFutures = nil
	p.stringMapFutures = nil
	p.int64Futures = nil
}

var _ fredis.IPipeline = (*pipeline)(nil)
