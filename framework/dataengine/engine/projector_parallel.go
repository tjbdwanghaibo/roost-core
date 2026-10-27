package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
)

// 按实体分片的并行投影（设计：docs/maintenance/FEATURE-2026-10-27-projector-entity-parallel.md）。
//
// 一轮回放读到的记录按冲突键分成互不相干的连通分量；分量数 ≥ 2 时分给至多 ProjectionWorkers 条通道并发投影，
// 每条通道内按 WAL 顺序合批。同一分量只在一条通道里，所以写同一文档（及共享回执、协调文档、effect 顺序类）
// 的记录保持 WAL 顺序。窗口结束后只确认连续成功前缀；前缀之后成功的记录靠持久事务标记在下一轮幂等重放。

// conflictKeyKind 区分冲突键的命名空间，避免实体 ID、回执身份、effect ID 互相碰撞。
type conflictKeyKind uint8

const (
	conflictEntity      conflictKeyKind = iota + 1 // 文档 _id（DAO 共用完整 Entity ID）
	conflictReceipt                                // 回执身份 Namespace/ID；lease fence 回执的 ID 就是协调文档身份
	conflictEffect                                 // effect ID（outbox 身份）
	conflictTransaction                            // 事务 ID
	conflictClass                                  // 整类记录之间保持顺序（effect 暂存顺序、不支持并发的 Remote 适配器）
)

type conflictKey struct {
	kind conflictKeyKind
	id   int64
	name string
}

var (
	// effectOrderKey 让带 effect 的记录之间保持 WAL 顺序：outbox 暂存顺序与串行投影相同（设计 §5，放宽须维护者决定）。
	effectOrderKey = conflictKey{kind: conflictClass, name: "effects"}
	// serialRemoteKey：Store 没有声明 Remote 适配器可并发调用时，所有 Remote 记录之间串行。
	serialRemoteKey = conflictKey{kind: conflictClass, name: "remote"}
)

// projectionConflictKeys 返回记录的冲突键；ok 为假表示记录不能参与并行调度（屏障），交给串行路径按现有规则处理
// （任一 mutation 的 Key.ID 为 0、Remote 写与 lease fence 同在的退役记录）。
func projectionConflictKeys(record coredata.CommitRecord, remoteParallel bool) (keys []conflictKey, ok bool) {
	if mixesRemoteWithLeaseFence(record) {
		return nil, false
	}
	keys = make([]conflictKey, 0, len(record.Mutations)+len(record.Receipts)+len(record.Effects)+2)
	keys = append(keys, conflictKey{kind: conflictTransaction, name: string(record.ID[:])})
	for _, mutation := range record.Mutations {
		if mutation.Key.ID == 0 {
			return nil, false
		}
		// 按 _id 取键比按完整文档身份粗：同 ID 不同集合的文档也被串起来，只多串行、不会漏。
		// Remote mutation 的 Key.ID 等于 Remote.EntityID（ValidateMutation），其 DAO、删除、快照都属于该实体（RemoteCommit.Validate）。
		keys = append(keys, conflictKey{kind: conflictEntity, id: mutation.Key.ID})
		if mutation.Remote != nil && !remoteParallel {
			keys = append(keys, serialRemoteKey)
		}
	}
	for _, receipt := range record.Receipts {
		keys = append(keys, conflictKey{kind: conflictReceipt, name: receipt.Namespace + "/" + receipt.ID})
	}
	for _, effect := range record.Effects {
		keys = append(keys, conflictKey{kind: conflictEffect, name: effect.ID})
	}
	if len(record.Effects) > 0 {
		keys = append(keys, effectOrderKey)
	}
	return keys, true
}

// replayWindow 是一轮回放里的一段连续记录 [start, end)。lanes 为空时整段走串行路径；
// 否则 lanes[i] 是第 i 条通道按 WAL 顺序排列的段内下标，fastPath[j] 表示段内第 j 条记录在并行通道里可以走单 mutation 快路。
type replayWindow struct {
	start, end int
	lanes      [][]int
	fastPath   []bool
}

// planReplayWindows 把一轮回放的记录切成串行段与并行窗口。屏障记录独占串行段；一段内互相冲突（分量数 < 2）
// 或 workers ≤ 1 时整段串行，行为与改动前相同。相邻的串行段合并，保持串行路径的合批。
func planReplayWindows(records []coredata.CommitRecord, workers int, remoteParallel bool) []replayWindow {
	if workers <= 1 || len(records) < 2 {
		return []replayWindow{{start: 0, end: len(records)}}
	}
	var windows []replayWindow
	appendSerial := func(start, end int) {
		if n := len(windows); n > 0 && windows[n-1].lanes == nil && windows[n-1].end == start {
			windows[n-1].end = end
			return
		}
		windows = append(windows, replayWindow{start: start, end: end})
	}
	keys := make([][]conflictKey, len(records))
	runStart := 0
	flushRun := func(end int) {
		if end <= runStart {
			return
		}
		if window, ok := planParallelWindow(keys[runStart:end], records[runStart:end], workers); ok {
			window.start, window.end = runStart, end
			windows = append(windows, window)
		} else {
			appendSerial(runStart, end)
		}
	}
	for i := range records {
		var ok bool
		keys[i], ok = projectionConflictKeys(records[i], remoteParallel)
		if !ok {
			flushRun(i)
			appendSerial(i, i+1)
			runStart = i + 1
		}
	}
	flushRun(len(records))
	return windows
}

// planParallelWindow 对一段无屏障记录做并查集，分量数 ≥ 2 时把分量按首条记录的 WAL 位置依次分给当前记录数最少的通道
// （整分量不拆，每条通道内保持 WAL 顺序），返回 ok=false 表示整段应串行。
func planParallelWindow(keys [][]conflictKey, records []coredata.CommitRecord, workers int) (replayWindow, bool) {
	parent := make([]int, len(records))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	owner := make(map[conflictKey]int)
	last := make(map[conflictKey]int)
	for i := range records {
		for _, key := range keys[i] {
			last[key] = i
			if first, seen := owner[key]; seen {
				if a, b := find(first), find(i); a != b {
					// 根取较小下标，分量的根就是它在 WAL 里的第一条记录。
					parent[max(a, b)] = min(a, b)
				}
				continue
			}
			owner[key] = i
		}
	}
	size := make(map[int]int)
	for i := range records {
		size[find(i)]++
	}
	if len(size) < 2 {
		return replayWindow{}, false
	}
	laneCount := min(workers, len(size))
	lanes := make([][]int, laneCount)
	load := make([]int, laneCount)
	laneOf := make(map[int]int, len(size))
	for i := range records {
		component := find(i)
		lane, assigned := laneOf[component]
		if !assigned {
			lane = 0
			for candidate := 1; candidate < laneCount; candidate++ {
				if load[candidate] < load[lane] {
					lane = candidate
				}
			}
			laneOf[component] = lane
			load[lane] += size[component]
		}
		lanes[lane] = append(lanes[lane], i)
	}
	// 单 mutation 快路不写事务标记，幂等只靠 _last_tx。只有本窗口里它的所有键都不再出现（之后的同文档写只可能在
	// 下一轮、且下一轮从确认前缀重读、它先被重放）时才允许走快路；否则进批量路径写标记（设计 §4 第 5 点）。
	fastPath := make([]bool, len(records))
	for i := range records {
		if !projectsOnFastPath(records[i]) {
			continue
		}
		fastPath[i] = true
		for _, key := range keys[i] {
			if last[key] != i {
				fastPath[i] = false
				break
			}
		}
	}
	return replayWindow{lanes: lanes, fastPath: fastPath}, true
}

// projectParallelWindow 并发执行一个并行窗口，返回 WAL 顺序上连续成功的记录数（相对窗口起点）。
// 任一通道失败即停止补位：各通道在下一单元前停下，已发出的单元等它返回；每个失败单独判定 fatal。
// 通道因逐条退回让一条“不是本窗口最后一条”的快路记录成功时只停该通道（prefix 小于窗口长度、err 为 nil），
// 调用方结束本轮，下一轮从确认前缀重读。
func (projector *Projector) projectParallelWindow(ctx context.Context, records []coredata.CommitRecord, fences []corenest.CommitFence, window replayWindow) (int, error) {
	done := make([]bool, len(records))
	laneErrs := make([]error, len(window.lanes))
	var stop atomic.Bool
	var wg sync.WaitGroup
	for lane, indexes := range window.lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := projector.projectLane(ctx, records, fences, indexes, window.fastPath, done, &stop); err != nil {
				laneErrs[lane] = err
				stop.Store(true)
				if projector.laneFailed != nil {
					projector.laneFailed()
				}
			}
		}()
	}
	wg.Wait()
	metrics.IncCounter("dataengine.projector.parallel_window.total", nil, 1)
	metrics.IncCounter("dataengine.projector.parallel_window.lanes.total", nil, int64(len(window.lanes)))
	metrics.IncCounter("dataengine.projector.parallel_window.records.total", nil, int64(len(records)))
	projector.parallelWindows.Add(1)
	projector.parallelLanes.Add(uint64(len(window.lanes)))
	prefix := 0
	for prefix < len(records) && done[prefix] {
		prefix++
	}
	var failures []error
	for _, err := range laneErrs {
		if err != nil {
			// 逐个判定：合并后的错误里若有一个“退回逐条”的路由哨兵，会让 isFatalProjection 忽略另一通道的冲突。
			projector.isFatalProjection(err)
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 && ctx.Err() == nil {
		if prefix < len(records) {
			metrics.IncCounter("dataengine.projector.parallel_window.truncated.total", nil, 1)
		}
		return prefix, nil
	}
	cause := errors.Join(append(failures, ctx.Err())...)
	return prefix, fmt.Errorf("dataengine projector: parallel window stopped after %d/%d records: %w", prefix, len(records), cause)
}

// projectLane 按 WAL 顺序投影一条通道的记录：连续普通本地记录合成 ProjectBatch（写事务标记），特殊记录单条 ProjectFenced；
// 只有 fastPath 允许的独占单条记录走快路。成功单元的收尾（驱逐 / 完成通知 / effect 唤醒 / 计数）与串行路径相同。
func (projector *Projector) projectLane(ctx context.Context, records []coredata.CommitRecord, fences []corenest.CommitFence, indexes []int, fastPath, done []bool, stop *atomic.Bool) error {
	laneRecords := make([]coredata.CommitRecord, len(indexes))
	laneFences := make([]corenest.CommitFence, len(indexes))
	for i, index := range indexes {
		laneRecords[i], laneFences[i] = records[index], fences[index]
	}
	segments, err := planProjectionSegments(laneRecords, laneFences, projector.opts.ReplayBatchRecords, projector.opts.ReplayBatchBytes)
	if err != nil {
		return err
	}
	batchStore := projector.store.(BatchProjectionStore)
	offset := 0
	for n, segment := range segments {
		unitIndexes := indexes[offset : offset+len(segment.records)]
		offset += len(segment.records)
		// 每条通道的第一个单元总会发出（与原 Remote 窗口“先发满一批”相同），哪些记录被尝试过因此不取决于
		// goroutine 的启动先后；之后的单元在首次观察到失败后不再发出。
		if ctx.Err() != nil || (n > 0 && stop.Load()) {
			return nil
		}
		projectStart := hoptrace.Now()
		outcome := projectionApplied
		if segment.batch && !(len(segment.records) == 1 && fastPath[unitIndexes[0]]) {
			err = batchStore.ProjectBatch(ctx, segment.records)
		} else {
			outcome, err = projector.projectSegment(ctx, segment)
		}
		if errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
			// 批量写无法自行判定（多半是重放已应用的记录）：本单元逐条走单条路径，由它比较已存版本与 _last_tx。
			for i := range segment.records {
				if stop.Load() || ctx.Err() != nil {
					return nil
				}
				single := projectionSegment{records: segment.records[i : i+1], fences: segment.fences[i : i+1]}
				singleStart := hoptrace.Now()
				outcome, err := projector.projectSegment(ctx, single)
				if err != nil {
					return err
				}
				projector.finishProjectedUnit(single.records, unitIndexes[i:i+1], outcome, singleStart, done)
				if projectsOnFastPath(single.records[0]) && !fastPath[unitIndexes[i]] {
					// 快路成功但本窗口后面还有同键记录：再往下投影，崩溃后重放这条会看到更高版本而误报冲突。本通道到此为止。
					return nil
				}
			}
			continue
		}
		if err != nil {
			if len(segment.records) > 1 {
				return fmt.Errorf("dataengine projector: segment first_transaction=%s records=%d: %w", segment.records[0].ID.String(), len(segment.records), err)
			}
			if segment.batch && !fastPath[unitIndexes[0]] {
				return fmt.Errorf("transaction %s: %w", segment.records[0].ID.String(), err)
			}
			return err
		}
		projector.finishProjectedUnit(segment.records, unitIndexes, outcome, projectStart, done)
	}
	return nil
}

// finishProjectedUnit 是一个投影单元成功后的收尾，串行与并行路径共用：逐跳计时、effect 唤醒 outbox、被跳过的原生步骤
// 交给驱逐（屏障与等待方到驱逐完成才解除）、否则完成投影通知，最后计数。done 非空时按 indexes 标记已完成（并行窗口算前缀用）。
func (projector *Projector) finishProjectedUnit(records []coredata.CommitRecord, indexes []int, outcome projectionOutcome, start time.Time, done []bool) {
	if !start.IsZero() {
		traceProjectedUnit(records, outcome, start)
	}
	if outcome != projectionSkipped && projector.opts.OnEffectsStaged != nil && hasEffects(records) {
		// effect 已随投影事务暂存：唤醒 outbox，不等它的轮询（不改投影 / checkpoint 的顺序与结果）。并行窗口里可能被多个通道同时调用。
		projector.opts.OnEffectsStaged()
	}
	for i := range records {
		if !projector.startStaleEviction(records[i].ID, outcome) {
			projector.completeProjection(records[i].ID, nil)
		}
	}
	projector.projected.Add(uint64(len(records)))
	for _, index := range indexes {
		done[index] = true
	}
}
