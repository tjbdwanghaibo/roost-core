package saga

import "errors"

// saga 的错误值。
var (
	ErrInvalidDefinition = errors.New("saga: invalid definition")
	ErrInvalidRecord     = errors.New("saga: invalid record")
	ErrDefinitionMissing = errors.New("saga: definition not registered")
	ErrAlreadyExists     = errors.New("saga: business operation already exists")
	ErrNotFound          = errors.New("saga: not found")
	ErrConflict          = errors.New("saga: optimistic concurrency conflict")
	ErrIdentityConflict  = errors.New("saga: idempotency identity conflict")
	ErrNotWaiting        = errors.New("saga: step is not waiting for a result")
	ErrDeadlineExpired   = errors.New("saga: resume requires a future or explicitly cleared deadline")
	ErrClosed            = errors.New("saga: engine is closed")
	// ErrSagaCompleted：Completed 的 saga 不能再人工 Compensate（维护者 2026-10-07 选 A）。
	ErrSagaCompleted = errors.New("saga: completed saga cannot be compensated")
	// ErrDurabilityUnconfirmed：写已在权威 Redis 上执行，但同连接的 WAITAOF 没有确认它落盘（部署违反 appendonly yes /
	// appendfsync always）。结果按“已执行、持久未确认”处理：不回滚、不报成功，进程经 RuntimeFailure fail-stop。
	ErrDurabilityUnconfirmed = errors.New("saga: write executed but durability unconfirmed")
	// ErrBackpressure：投影积压超过上限（或权威 Redis 内存满），新 saga 暂不接受；可重试，已存在的 saga 照常推进。
	ErrBackpressure = errors.New("saga: backpressure, retry later")
	// ErrRedisAuthorityMismatch：启动守卫发现权威 Redis 与 Mongo 冷副本不是同一套（Redis 被清空、换了库、格式或分区不符），
	// 拒绝启动；按灾备手册处理，不自动把空 Redis 当权威。
	ErrRedisAuthorityMismatch = errors.New("saga: authority redis does not match the mongo copy")
)
