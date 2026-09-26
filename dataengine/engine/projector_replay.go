package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
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
	if len(records) == 0 {
		if errors.Is(replayErr, errProjectorBatchComplete) {
			replayErr = nil
		}
		return 0, replayErr
	}
	if errors.Is(replayErr, errProjectorBatchComplete) {
		replayErr = nil
	}
	multiStore, multi := projector.store.(MultiMutationBatchProjectionStore)
	multi = multi && multiStore.SupportsMultiMutationBatch()
	segments, err := planProjectionSegments(records, fences, projector.opts.ReplayBatchRecords, projector.opts.ReplayBatchBytes, multi)
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
		// perRecord is latched when a batch reports that it cannot classify
		// its own outcome (ErrProjectionBatchNeedsPerRecord). The rest of the
		// segment then goes through the single-record path, which compares the
		// stored version and _last_tx and so can tell an already-applied
		// replay from a real conflict. Latching rather than retrying just the
		// failing unit keeps the pass from re-issuing a bulk write that is
		// going to defer again.
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
			if err := projector.projectSegment(ctx, unit); err != nil {
				if errors.Is(err, ErrProjectionBatchNeedsPerRecord) && len(unit.records) > 1 {
					perRecord = true
					continue
				}
				projector.isFatalProjection(err)
				return processed, fmt.Errorf("dataengine projector: segment first_transaction=%s records=%d: %w", unit.records[0].ID.String(), len(unit.records), err)
			}
			for i := range unit.records {
				projector.completeProjection(unit.records[i].ID, nil)
			}
			projector.projected.Add(uint64(len(unit.records)))
			processed += len(unit.records)
			// 单 mutation 的 Project 快路只保存文档末次事务 ID。若后续版本
			// 覆盖它，旧记录便无法判定幂等，因此仍立即 ack；未知 Store 也保留旧契约。
			replaySafe := multi && (len(unit.records) > 1 || len(unit.records[0].Mutations) > 1)
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

func (projector *Projector) projectSegment(ctx context.Context, segment projectionSegment) error {
	if len(segment.records) == 1 {
		if err := projector.store.Project(ctx, segment.records[0]); err != nil {
			return fmt.Errorf("transaction %s: %w", segment.records[0].ID.String(), err)
		}
		return nil
	}
	if store, ok := projector.store.(BatchProjectionStore); ok {
		return store.ProjectBatch(ctx, segment.records)
	}
	for i := range segment.records {
		if err := projector.store.Project(ctx, segment.records[i]); err != nil {
			return fmt.Errorf("transaction %s: %w", segment.records[i].ID.String(), err)
		}
	}
	return nil
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
