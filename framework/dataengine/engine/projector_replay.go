package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ReplayPass 按 WAL 物理顺序读取，在仍持有 Entity 锁的事务前停下。
// 分段投影成功后才推进 ack；Mongo 已提交而 ack 失败时，下次依靠幂等身份重放。
func (projector *Projector) ReplayPass(ctx context.Context) (processed int, resultErr error) {
	if !projector.operations.Begin() {
		return 0, ErrRuntimeStopped
	}
	defer projector.operations.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := projector.replayGate.acquire(ctx); err != nil {
		return 0, err
	}
	defer projector.replayGate.release()
	records, fences, replayErr := projector.readReplayBatch(ctx)
	if len(records) == 0 {
		return 0, replayErr
	}
	segments, err := planProjectionSegments(records, fences, projector.opts.ReplayBatchRecords, projector.opts.ReplayBatchBytes)
	if err != nil {
		return 0, err
	}
	_, batchCapable := projector.store.(BatchProjectionStore)
	// processed 是已成功投影的连续前缀；acked 只在 checkpoint 成功后前移。
	acked := 0
	lastAck := projector.now()
	checkpoint := func() error {
		if processed == acked {
			return nil
		}
		if err := projector.ack(ctx, fences[processed-1]); err != nil {
			projector.observeAckError(err)
			return err
		}
		projector.acknowledge(records[acked:processed])
		acked = processed
		lastAck = projector.now()
		return nil
	}
	// 所有退出路径都只尝试确认已成功的前缀。ack 失败不能被 held 哨兵掩盖。
	ackFailed := false
	defer func() {
		if ackFailed {
			return
		}
		if err := checkpoint(); err != nil {
			if errors.Is(resultErr, errProjectorTransactionHeld) {
				resultErr = err
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	parallelStore, parallel := projector.store.(RemoteParallelProjectionStore)
	parallel = parallel && parallelStore.SupportsRemoteParallelProjection() && projector.opts.RemoteProjectionWorkers > 1
	for segmentIndex := 0; segmentIndex < len(segments); {
		if parallel {
			count := remoteProjectionWindow(segments[segmentIndex:], projector.opts.ReplayBatchRecords, projector.opts.ReplayBatchBytes)
			if count > 1 {
				if err := checkpoint(); err != nil {
					ackFailed = true
					return processed, err
				}
				prefix, err := projector.projectRemoteWindow(ctx, segments[segmentIndex:segmentIndex+count])
				processed += prefix
				if err != nil {
					// fatal 由 isFatalProjection 统一唤醒全部待投影等待方。
					projector.isFatalProjection(err)
					return processed, err
				}
				if err := checkpoint(); err != nil {
					ackFailed = true
					return processed, err
				}
				segmentIndex += count
				continue
			}
		}
		segment := segments[segmentIndex]
		segmentIndex++
		special := !isLocalBatchRecord(segment.records[0])
		if special {
			if err := checkpoint(); err != nil {
				ackFailed = true
				return processed, err
			}
		}
		// 批量写报告无法自行判定结果（ErrProjectionBatchNeedsPerRecord）时锁定 perRecord：
		// 本段剩余记录改走单条路径，由它比较已存版本与 _last_tx，区分已应用的重放和
		// 真实冲突。锁定而不是只重试失败单元，避免反复发出注定再次退回的批量写。
		perRecord := false
		for start := 0; start < len(segment.records); {
			end := len(segment.records)
			if segment.batch && (!batchCapable || perRecord) {
				end = start + 1
			}
			unit := projectionSegment{
				records: segment.records[start:end],
				fences:  segment.fences[start:end],
				batch:   segment.batch,
			}
			outcome, err := projector.projectSegment(ctx, unit)
			if err != nil {
				if errors.Is(err, ErrProjectionBatchNeedsPerRecord) && len(unit.records) > 1 {
					perRecord = true
					continue
				}
				projector.isFatalProjection(err)
				return processed, fmt.Errorf("dataengine projector: segment first_transaction=%s records=%d: %w", unit.records[0].ID.String(), len(unit.records), err)
			}
			for i := range unit.records {
				// 被跳过的原生步骤先驱逐内存实体，屏障与等待方到驱逐完成才解除。
				if !projector.startStaleEviction(unit.records[i].ID, outcome) {
					projector.completeProjection(unit.records[i].ID, nil)
				}
			}
			projector.projected.Add(uint64(len(unit.records)))
			processed += len(unit.records)
			// 单 mutation 的 Project 快路只保存文档末次事务 ID。若后续版本
			// 覆盖它，旧记录便无法判定幂等，因此仍立即 ack；非批量 Store 也逐笔确认。
			replaySafe := batchCapable && (len(unit.records) > 1 || len(unit.records[0].Mutations) > 1)
			if special || !replaySafe || processed-acked >= projector.opts.CheckpointRecords || projector.now().Sub(lastAck) >= projector.opts.CheckpointInterval {
				if err := checkpoint(); err != nil {
					ackFailed = true
					return processed, err
				}
			}
			start = end
		}
	}
	return processed, replayErr
}

// readReplayBatch 按 WAL 物理顺序收集本轮可投影的连续记录：遇到仍持有实体锁的事务、
// 达到条数或读取字节上限时停止。返回的错误已去掉“批次已满”哨兵，可能是
// errProjectorTransactionHeld 或 WAL 读取错误；记录为空时调用方不做投影。
func (projector *Projector) readReplayBatch(ctx context.Context) ([]corenest.CommitRecord, []corenest.CommitFence, error) {
	var records []corenest.CommitRecord
	var fences []corenest.CommitFence
	readBytes := 0
	replayErr := projector.wal.Replay(ctx, func(fence corenest.CommitFence, record corenest.CommitRecord) error {
		// 上一条已被本轮接收，必须让它的 consume 返回 nil，WAL 才能如实计数。
		// 读到下一条时停止；这条 lookahead 不保留、不确认。
		if len(records) >= projector.opts.ReplayBatchRecords || readBytes >= projector.opts.ReplayReadBytes {
			return errProjectorBatchComplete
		}
		if projector.isHeld(record.ID) {
			return errProjectorTransactionHeld
		}
		size := projectionRecordLogicalBytes(record)
		if len(records) > 0 && size > projector.opts.ReplayReadBytes-readBytes {
			// WAL 已解码这一条，但不保留、不确认它；下一轮从成功前缀后重新读取。
			return errProjectorBatchComplete
		}
		// 空闲或首条事务尚未解锁时不需要批次空间。等到确实有可投影
		// 记录才分配，避免后台每次无进展轮询都申请整个批次的容量。
		if records == nil {
			records = make([]corenest.CommitRecord, 0, projector.opts.ReplayBatchRecords)
			fences = make([]corenest.CommitFence, 0, projector.opts.ReplayBatchRecords)
		}
		records = append(records, record)
		fences = append(fences, fence)
		readBytes = saturatingAdd(readBytes, size)
		return nil
	})
	if errors.Is(replayErr, errProjectorBatchComplete) {
		replayErr = nil
	}
	return records, fences, replayErr
}

// projectSegment 返回的 outcome 只对单笔段有意义：带 lease fence 的记录总是单独成段
// （isLocalBatchRecord 排除 receipt）。批量段只含普通本地记录，结果总是 applied。
func (projector *Projector) projectSegment(ctx context.Context, segment projectionSegment) (projectionOutcome, error) {
	if len(segment.records) == 1 {
		record := segment.records[0]
		// 退役的历史记录：不交给 Store，投影器 fatal，checkpoint 停在它之前。
		if mixesRemoteWithLeaseFence(record) {
			return projectionApplied, fmt.Errorf("transaction %s: %w", record.ID.String(), ErrRetiredRemoteLeaseFenceRecord)
		}
		outcome := projectionApplied
		var err error
		if store, ok := projector.store.(FenceOutcomeProjectionStore); ok {
			var skipped bool
			if skipped, err = store.ProjectFenced(ctx, record); skipped {
				outcome = projectionSkipped
			}
		} else {
			err = projector.store.Project(ctx, record)
			if hasLeaseFence(record) {
				outcome = projectionOutcomeUnknown
			}
		}
		if err != nil {
			return projectionApplied, fmt.Errorf("transaction %s: %w", record.ID.String(), err)
		}
		return outcome, nil
	}
	if store, ok := projector.store.(BatchProjectionStore); ok {
		return projectionApplied, store.ProjectBatch(ctx, segment.records)
	}
	for i := range segment.records {
		if err := projector.store.Project(ctx, segment.records[i]); err != nil {
			return projectionApplied, fmt.Errorf("transaction %s: %w", segment.records[i].ID.String(), err)
		}
	}
	return projectionApplied, nil
}

func (projector *Projector) run() {
	defer close(projector.done)
	backoff := projector.opts.RetryMin
	for {
		processed, err := projector.ReplayPass(projector.ctx)
		if errors.Is(err, errProjectorTransactionHeld) {
			backoff = projector.opts.RetryMin
			if !projector.wait(projector.opts.RetryMin) {
				return
			}
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			projector.recordFailure(err)
			if projector.isFatalProjection(err) {
				return
			}
			if !projector.wait(jitterDuration(backoff)) {
				return
			}
			backoff = min(backoff*2, projector.opts.RetryMax)
			continue
		}
		if projector.ctx.Err() != nil {
			return
		}
		projector.setLastError(nil)
		backoff = projector.opts.RetryMin
		if processed > 0 {
			continue
		}
		if !projector.wait(projector.opts.IdlePoll) {
			return
		}
	}
}

func (projector *Projector) acknowledge(records []coredata.CommitRecord) {
	projector.heldMu.Lock()
	for _, record := range records {
		id := record.ID
		if _, ok := projector.admitted[id]; !ok {
			continue
		}
		delete(projector.admitted, id)
		projector.walUnacked.Add(^uint64(0))
	}
	metrics.SetGauge("dataengine.projection.pending", nil, int64(projector.walUnacked.Load()))
	projector.heldMu.Unlock()
}

func (projector *Projector) wait(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-projector.kick:
		return true
	case <-timer.C:
		return true
	case <-projector.ctx.Done():
		return false
	}
}

func jitterDuration(duration time.Duration) time.Duration {
	if duration <= 1 {
		return duration
	}
	half := duration / 2
	return half + time.Duration(rand.Int64N(int64(duration-half)))
}
