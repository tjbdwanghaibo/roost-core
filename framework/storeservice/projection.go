// Package storeservice 是权威 Redis 到 Mongo 冷副本的通用投影运行器（Store Service）。
//
// 权威数据在单主 Redis 上，每次状态变化同时往所属分区的日志（流 <前缀>:log:<p>）追加一条条目。运行器按分区并行：每个
// （投影，分区）一个循环，取分区租约、从进度（<前缀>:proj:<p>）之后读一批条目、交给领域投影写进 Mongo、再用脚本推进进度
// 并裁剪已投影的条目。领域投影（文档形状、驱逐）由各领域自己实现（saga 的 RedisProjection），运行器只管分区、批、进度、
// 暂停与指标。
//
// 正确性不依赖租约排他：Mongo 写按版本单调、幂等，进度只能由持有租约的实例推进（advance.lua 核对 owner + token）；
// 租约只是避免几个实例重复劳动，所以这里不需要 App 级单实例保证。日志条目只在 Mongo 确认之后裁剪（先保存后回收）。
package storeservice

import (
	"context"
	"errors"
)

// Entry 是分区日志里的一条条目。
type Entry struct {
	ID     string // 流条目 ID（毫秒-序号）
	Fields map[string]string
}

// Projection 是一个领域的投影：把它在权威 Redis 上的日志写进 Mongo 冷副本。
type Projection interface {
	// Name 是投影名（"saga"；之后的 Remote、实体投影各自一个），用作指标标签。
	Name() string
	// KeyPrefix 是这个领域在权威 Redis 上的键前缀，运行器由它派生 log / proj / plock / paused 键。
	KeyPrefix() string
	// Partitions 是分区数。
	Partitions() int
	// Apply 把一批条目写进 Mongo。必须幂等、按版本单调：重放、乱序重放都不能让副本倒退。返回 Permanent(err) 表示非暂时
	// 错误（唯一索引冲突、文档校验失败），运行器暂停该分区、不跳过；其他错误按暂时错误退避后重试同一批。
	Apply(ctx context.Context, partition int, entries []Entry) error
	// AfterAdvance 在进度推进之后调用（空闲时也定期调用，upto 是当前进度），saga 用它驱逐已投影、过了保留期的终态记录。
	// 出错只记录，下一轮再做。
	AfterAdvance(ctx context.Context, partition int, upto string) error
}

// permanentError 标记非暂时错误。
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent 把 err 标记为非暂时错误：重试同一批不会成功，运行器暂停分区等运维核对。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent 报告 err 是否经 Permanent 标记。
func IsPermanent(err error) bool {
	var target permanentError
	return errors.As(err, &target)
}
