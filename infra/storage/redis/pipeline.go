package redis

import (
	"context"
	"time"
)

// IPipeline batches commands. Cluster clients route batches per node;
// execution is not a transaction or an atomic snapshot across keys.
type IPipeline interface {
	Get(ctx context.Context, key string) *FutureBytes
	Set(ctx context.Context, key string, value any, expiration time.Duration)
	Del(ctx context.Context, keys ...string)
	HSet(ctx context.Context, key string, values ...any)
	HGet(ctx context.Context, key, field string) *FutureBytes
	HGetAll(ctx context.Context, key string) *FutureStringMap
	Incr(ctx context.Context, key string) *FutureInt64
	Expire(ctx context.Context, key string, expiration time.Duration)
	ZAdd(ctx context.Context, key string, members ...Z)
	RPush(ctx context.Context, key string, values ...any)
	LPop(ctx context.Context, key string) *FutureBytes
	// Eval 把一条脚本排进 pipeline（每条命令发脚本原文）。Cluster 客户端按脚本的第一个键路由，所以每条脚本只能碰
	// 同一槽位的键。与 IRedis.Eval 不同，排队的脚本属于写命令：回复丢失时不经驱动重放，Exec 只在整条确定没执行时重发。
	Eval(ctx context.Context, script string, keys []string, args ...any) *FutureAny

	// Exec sends buffered commands and populates futures. Inspect each future
	// for its command's error even when Exec returns nil.
	// Missing reads alone are not a batch failure; other command errors are.
	// An error does not roll back commands that already executed.
	Exec(ctx context.Context) error

	// Discard clears buffered commands without executing.
	Discard()
}

// FutureBytes holds a deferred []byte result from pipeline.
type FutureBytes struct {
	val []byte
	err error
}

func NewFutureBytes(val []byte, err error) *FutureBytes {
	return &FutureBytes{val: val, err: err}
}

func (f *FutureBytes) Result() ([]byte, error) { return f.val, f.err }

func (f *FutureBytes) SetResult(val []byte, err error) {
	f.val = val
	f.err = err
}

// FutureAny holds a deferred script reply from pipeline (the same value
// IRedis.Eval returns: int64, string, []any or nil).
type FutureAny struct {
	val any
	err error
}

func (f *FutureAny) Result() (any, error) { return f.val, f.err }

func (f *FutureAny) SetResult(val any, err error) {
	f.val = val
	f.err = err
}

// FutureStringMap holds a deferred map[string]string result from pipeline.
type FutureStringMap struct {
	val map[string]string
	err error
}

func (f *FutureStringMap) Result() (map[string]string, error) { return f.val, f.err }

func (f *FutureStringMap) SetResult(val map[string]string, err error) {
	f.val = val
	f.err = err
}

// FutureInt64 holds a deferred int64 result from pipeline.
type FutureInt64 struct {
	val int64
	err error
}

func NewFutureInt64(val int64, err error) *FutureInt64 {
	return &FutureInt64{val: val, err: err}
}

func (f *FutureInt64) Result() (int64, error) { return f.val, f.err }

func (f *FutureInt64) SetResult(val int64, err error) {
	f.val = val
	f.err = err
}
