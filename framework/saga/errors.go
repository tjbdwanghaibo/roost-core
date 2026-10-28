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

	// 以下是 JetStream 协调器（NewJetStreamEngine，第 2 批并存、未接线）的错误，设计见
	// docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §2.1。

	// ErrStartUnconfirmed：启动（或 Resume / Compensate）意图已经持久进入分区输入，但在调用方 ctx 内没等到所有者的判定。
	// 结果未知，不是失败：意图仍在流里，所有者会处理它；用同一请求重试是安全的（同一意图按启动摘要判“已存在”）。
	// 返回的错误同时满足 errors.Is(err, ctx.Err())。
	ErrStartUnconfirmed = errors.New("saga: intent persisted but the owner's verdict was not received")
	// ErrStartIdentityReused：启动尝试标识第一次发布就被流按 MsgID 判成重复——标识撞上了去重窗口里的另一次尝试，意图没有入流。
	// 标识全局唯一（RV-20261028-01），出现说明标识生成坏了，不能当成功返回。
	ErrStartIdentityReused = errors.New("saga: start attempt identity already used within the duplicate window")
	// ErrPartitionUnavailable：查询要的分区所有者在超时内没有回答（List 不返回残缺结果）。错误文本点名分区。
	ErrPartitionUnavailable = errors.New("saga: partition owner unavailable")
	// ErrReceiptsExpired：记录最后一次更新早于参与方回执保留期（扣掉最长步骤窗口），Resume / 补偿方向的 Compensate 重新执行的
	// 操作可能已经没有回执、会再生效一次，所以拒绝（选项 RR-A）。运维先核对业务数据再走业务冲正。
	ErrReceiptsExpired = errors.New("saga: step receipts may have expired; resume refused")
	// ErrSnapshotIncompatible：分区快照的版本、分区、布局、长度或摘要与本进程不符，分区不启动；不退回从头重放（日志已按快照清理）。
	ErrSnapshotIncompatible = errors.New("saga: partition snapshot incompatible")
	// ErrLayoutMismatch：流与桶里登记的分区布局（分区数、流数、前缀）与本进程的配置不同，拒绝启动。
	ErrLayoutMismatch = errors.New("saga: partition layout mismatch")
	// ErrPartitionFenced：分区事件的期望序号被别的写者推进了（崩溃重启的短暂重叠或部署错误），本进程经 RuntimeFailure fail-stop。
	ErrPartitionFenced = errors.New("saga: partition fenced by another writer")
	// ErrPublishUnknown：分区事件发布结果未知，重发判定超过预算仍没有结论；经 RuntimeFailure fail-stop。
	ErrPublishUnknown = errors.New("saga: partition publish outcome unknown past its budget")
	// ErrPartitionRejected：服务端明确拒绝了分区事件（流满 MaxBytes + Discard=New、消息超限等），确定没写入；不当结果未知重试，
	// 经 RuntimeFailure fail-stop 并告警。
	ErrPartitionRejected = errors.New("saga: partition publish rejected by the server")
)
