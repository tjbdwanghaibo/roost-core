package saga

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// partition_replay.go 是分区的启动（快照 + 尾巴重放）、快照与清理（设计 §4.5、§10）。
//
// 快照是一轮结束时的已提交状态：清单（KV 键 p<k>，按修订号 CAS）记 LastEv / InHi 与正文对象名、长度、SHA-256；正文在对象桶里。
// 重放：读清单 → 校验版本 / 分区 / 布局（不符 ErrSnapshotIncompatible，分区不启动，不退回从头重放——日志已按快照清理）→
// 读正文并核对长度与摘要 → 从 min(InHi, LastEv)+1 读到分区过滤上的最后一条（LastMsg 的序号，确定终点）：事件只取 LastEv 之后的、
// 按序覆盖；输入只留 InHi 之后的、交给循环重新处理。读完核对快照修订号与事件过滤的最后一条没变（期间有别的写者就重来）。
//
// 清理：purge(分区过滤, below = min(InHi+1, LastEv, 每个 durable 的 ack floor + 1))。保留最后一条事件（期望序号依赖它）与
// 重放起点之后的全部消息；不越过任何 durable 的 ack floor（不删参与方还没取走的命令），某个 durable 卡住超过 PurgeStallAlarm
// 时告警点名，不跳过它。

// replayPartition 重建分区 k 的内存状态。返回的分区还没开始消费输入（由 startPartition 接上 Follow 再起循环）。
func (e *Engine) replayPartition(ctx context.Context, k int) (*partition, error) {
	p := &partition{
		e: e, k: k, stream: e.layout.StreamOf(k), writer: e.writer,
		inputs: make(chan fnats.LogMsg, 4096), queries: make(chan pendingQuery, 1024),
		stopCh: make(chan struct{}), done: make(chan struct{}), snapshotSlot: make(chan struct{}, 1),
		purgeStalls: map[string]purgeStall{},
	}
	for attempt := 0; ; attempt++ {
		stable, err := p.replayOnce(ctx)
		if err != nil {
			return nil, err
		}
		if stable {
			break
		}
		if attempt >= 5 {
			return nil, fmt.Errorf("saga: partition %d kept changing during replay (another writer?)", k)
		}
	}
	now := e.now()
	p.wheel = newWheel(e.opts.Tick, e.opts.WheelSlots, now)
	var terminals []pruneEntry
	for id, st := range p.sagas {
		switch {
		case !st.Record.Status.Terminal():
			p.wheel.schedule(id, st.Record.NextRunAt)
		case prunable(st.Record.Status):
			terminals = append(terminals, pruneEntry{id: id, updatedAt: st.Record.UpdatedAt})
		}
	}
	slices.SortFunc(terminals, func(a, b pruneEntry) int { return a.updatedAt.Compare(b.updatedAt) })
	p.terminals = terminals
	p.lastSnapshotAt = now
	return p, nil
}

// replayOnce 做一遍重放；stable=false 表示期间快照或分区事件被别人改过，需要重来。
func (p *partition) replayOnce(ctx context.Context) (bool, error) {
	e := p.e
	p.sagas, p.lastEv, p.inHi, p.pending = map[string]*sagaState{}, 0, 0, nil
	p.snapshotRev, p.snapshotObject = 0, ""
	var snapLastEv, snapInHi uint64
	entry, err := e.snapshots.Get(ctx, snapshotKey(p.k))
	switch {
	case err == nil:
		manifest, err := decodeManifest(entry.Value, e.layout, p.k)
		if err != nil {
			return false, err
		}
		body, err := e.objects.Get(ctx, manifest.Object)
		if err != nil {
			return false, fmt.Errorf("%w: partition %d snapshot object %q: %v", ErrSnapshotIncompatible, p.k, manifest.Object, err)
		}
		states, err := decodeSnapshotBody(body, manifest, e.layout, p.k)
		if err != nil {
			return false, err
		}
		for i := range states {
			st := states[i]
			p.sagas[st.Record.ID] = &st
		}
		snapLastEv, snapInHi = manifest.LastEv, manifest.InHi
		p.lastEv, p.inHi = manifest.LastEv, manifest.InHi
		p.snapshotRev, p.snapshotObject = entry.Revision, manifest.Object
	case errors.Is(err, fnats.ErrLogNotFound):
	default:
		return false, fmt.Errorf("saga: partition %d snapshot manifest: %w", p.k, err)
	}
	end, err := e.lastSeq(ctx, p.stream, e.layout.PartitionFilter(p.k))
	if err != nil {
		return false, err
	}
	from := min(snapLastEv, snapInHi) + 1
	read := 0
	if end >= from {
		err := e.log.ReadRange(ctx, p.stream, []string{e.layout.PartitionFilter(p.k)}, from, end, func(msg fnats.LogMsg) error {
			read++
			if !e.layout.isEventSubject(p.k, msg.Subject) {
				if msg.Seq > snapInHi {
					p.pending = append(p.pending, msg)
				}
				return nil
			}
			if msg.Seq <= snapLastEv {
				return nil // 已在快照里
			}
			ev, err := decodeEvent(msg.Data, e.layout, p.k)
			if err != nil {
				return fmt.Errorf("saga: partition %d event at %d is unreadable (the authoritative log is not trustworthy): %w", p.k, msg.Seq, err)
			}
			st := ev.Saga
			p.sagas[st.Record.ID] = &st
			p.lastEv = msg.Seq
			p.inHi = max(p.inHi, ev.In)
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	// 只留水位之后的输入：之前的已经判定过（事件的 In 推进了水位）。
	p.pending = slices.DeleteFunc(p.pending, func(msg fnats.LogMsg) bool { return msg.Seq <= p.inHi })
	// 核对：重放期间快照没被换掉、分区事件没有被别人推进（否则重来；真有第二个写者时之后的发布会被隔离）。
	lastEvent, err := e.lastSeq(ctx, p.stream, e.layout.EventFilter(p.k))
	if err != nil {
		return false, err
	}
	again, err := e.snapshots.Get(ctx, snapshotKey(p.k))
	sameSnapshot := (err == nil && again.Revision == p.snapshotRev) || (errors.Is(err, fnats.ErrLogNotFound) && p.snapshotRev == 0)
	if err != nil && !errors.Is(err, fnats.ErrLogNotFound) {
		return false, fmt.Errorf("saga: partition %d snapshot manifest: %w", p.k, err)
	}
	if !sameSnapshot || lastEvent != p.lastEv {
		return false, nil
	}
	p.followFrom = max(end, p.inHi, p.lastEv) + 1
	e.stats.replayed.Add(uint64(read))
	slog.Info("saga: partition replayed", "partition", p.k, "sagas", len(p.sagas), "messages", read,
		"last_event", p.lastEv, "input_watermark", p.inHi, "pending_inputs", len(p.pending), "snapshot_revision", p.snapshotRev)
	return true, nil
}

// lastSeq 是流里匹配 filter 的最后一条消息的序号（leader 读），没有为 0。
func (e *Engine) lastSeq(ctx context.Context, stream, filter string) (uint64, error) {
	msg, err := e.log.LastMsg(ctx, stream, filter)
	switch {
	case err == nil:
		return msg.Seq, nil
	case errors.Is(err, fnats.ErrLogNotFound):
		return 0, nil
	default:
		return 0, fmt.Errorf("saga: last message of %s on %s: %w", filter, stream, err)
	}
}

// ---- 快照 ----

// snapshotView 是循环交给快照 goroutine 的一致视图：已提交的 *sagaState 提交后不再修改，所以浅拷贝 map 就是一致视图。
type snapshotView struct {
	sagas  []*sagaState
	lastEv uint64
	inHi   uint64
}

// maybeSnapshot 到点（每 SnapshotEveryEvents 条事件，或 SnapshotInterval 且有新事件）时交一个快照给后台 goroutine；
// 同一分区同时最多一个快照在途，在途时到点就跳过。快照不阻塞循环（设计 §4.5 相对原型的差异）。
func (p *partition) maybeSnapshot(ctx context.Context, now time.Time) {
	opts := p.e.opts
	if p.sinceSnapshot == 0 || (p.sinceSnapshot < opts.SnapshotEveryEvents && now.Sub(p.lastSnapshotAt) < opts.SnapshotInterval) {
		return
	}
	select {
	case p.snapshotSlot <- struct{}{}:
	default:
		return
	}
	view := snapshotView{sagas: make([]*sagaState, 0, len(p.sagas)), lastEv: p.lastEv, inHi: p.inHi}
	for _, st := range p.sagas {
		view.sagas = append(view.sagas, st)
	}
	p.sinceSnapshot, p.lastSnapshotAt = 0, now
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		defer func() { <-p.snapshotSlot }()
		if err := p.writeSnapshot(ctx, view, now); err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrPartitionFenced) {
				p.fail(err)
				return
			}
			metrics.IncCounter("saga.partition.snapshot_failures_total", nil, 1)
			slog.Error("saga: partition snapshot failed; the next one retries", "partition", p.k, "err", err)
			return
		}
		p.purge(ctx, view, now)
	}()
}

// writeSnapshot 先写正文对象 p<k>-<lastEv>，再按修订号 CAS 清单（首次 Create）。CAS 冲突时读回清单：别人的 LastEv 比自己新 →
// 本进程已被接管（ErrPartitionFenced）；否则在其上覆盖。成功后删上一个正文对象。
func (p *partition) writeSnapshot(ctx context.Context, view snapshotView, now time.Time) error {
	e := p.e
	slices.SortFunc(view.sagas, func(a, b *sagaState) int {
		switch {
		case a.Record.ID < b.Record.ID:
			return -1
		case a.Record.ID > b.Record.ID:
			return 1
		}
		return 0
	})
	states := make([]sagaState, len(view.sagas))
	for i, st := range view.sagas {
		states[i] = *st
	}
	body, err := json.Marshal(states)
	if err != nil {
		return fmt.Errorf("saga: encode snapshot: %w", err)
	}
	object := "p" + strconv.Itoa(p.k) + "-" + strconv.FormatUint(view.lastEv, 10)
	writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := e.objects.Put(writeCtx, object, body); err != nil {
		return fmt.Errorf("saga: put snapshot object %s: %w", object, err)
	}
	sum := sha256.Sum256(body)
	manifest := snapshotManifest{V: snapshotWireVersion, Part: p.k, Layout: layoutOf(e.layout), LastEv: view.lastEv, InHi: view.inHi,
		Writer: p.writer, At: now, Object: object, Size: len(body), SHA256: hex.EncodeToString(sum[:])}
	raw, _ := json.Marshal(manifest)
	key := snapshotKey(p.k)
	var rev uint64
	if p.snapshotRev == 0 {
		rev, err = e.snapshots.Create(writeCtx, key, raw)
	} else {
		rev, err = e.snapshots.Update(writeCtx, key, raw, p.snapshotRev)
	}
	if errors.Is(err, fnats.ErrLogKVConflict) {
		e.stats.snapshotConflicts.Add(1)
		entry, gerr := e.snapshots.Get(writeCtx, key)
		if gerr != nil {
			return fmt.Errorf("saga: snapshot conflict and reread failed: %v / %w", err, gerr)
		}
		var other snapshotManifest
		if jerr := json.Unmarshal(entry.Value, &other); jerr != nil {
			return fmt.Errorf("saga: snapshot conflict, unreadable manifest: %w", jerr)
		}
		if other.LastEv > view.lastEv {
			return fmt.Errorf("%w: partition %d has a newer snapshot (last event %d > %d, writer %s)", ErrPartitionFenced, p.k, other.LastEv, view.lastEv, other.Writer)
		}
		rev, err = e.snapshots.Update(writeCtx, key, raw, entry.Revision)
	}
	if err != nil {
		return fmt.Errorf("saga: write snapshot manifest: %w", err)
	}
	previous := p.snapshotObject
	p.snapshotRev, p.snapshotObject = rev, object
	e.stats.snapshots.Add(1)
	if previous != "" && previous != object {
		if err := e.objects.Delete(writeCtx, previous); err != nil && !errors.Is(err, fnats.ErrLogNotFound) {
			slog.Warn("saga: could not delete the previous snapshot object", "partition", p.k, "object", previous, "err", err)
		}
	}
	return nil
}

// purgeBound 是清理的上界（不含）：min(InHi+1, LastEv, 每个 durable 的 ack floor + 1)。保留最后一条事件（期望序号依赖它）、
// 重放起点之后的全部消息，以及任何 durable 还没确认的消息。
func purgeBound(view snapshotView, floors map[string]uint64) uint64 {
	bound := min(view.inHi+1, view.lastEv)
	for _, floor := range floors {
		bound = min(bound, floor+1)
	}
	return bound
}

// purge 在快照之后清理分区 k 的旧消息，并对卡住清理的 durable 告警。
func (p *partition) purge(ctx context.Context, view snapshotView, now time.Time) {
	e := p.e
	purgeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	floors, err := e.log.DurableAckFloors(purgeCtx, p.stream)
	if err != nil {
		slog.Warn("saga: purge skipped: cannot list durable consumers", "partition", p.k, "err", err)
		return
	}
	bound := purgeBound(view, floors)
	p.watchPurgeStalls(view, floors, now)
	if bound <= 1 {
		return
	}
	if err := e.log.Purge(purgeCtx, p.stream, e.layout.PartitionFilter(p.k), bound); err != nil {
		slog.Warn("saga: purge failed", "partition", p.k, "below", bound, "err", err)
		return
	}
	e.stats.purges.Add(1)
}

// watchPurgeStalls 记每个 durable 的 ack floor 卡在清理目标之下多久；超过 PurgeStallAlarm 告警一次（点名 durable），之后它前进了
// 就重新计时。下线的步骤要由运维删掉它的 durable。
func (p *partition) watchPurgeStalls(view snapshotView, floors map[string]uint64, now time.Time) {
	target := min(view.inHi+1, view.lastEv)
	for name, floor := range floors {
		stall, seen := p.purgeStalls[name]
		if floor+1 >= target {
			delete(p.purgeStalls, name)
			continue
		}
		if !seen || stall.floor != floor {
			p.purgeStalls[name] = purgeStall{floor: floor, since: now}
			continue
		}
		blocked := now.Sub(stall.since)
		metrics.SetGauge("saga.log.purge_blocked_seconds", metrics.Labels{"stream": p.stream, "consumer": name}, int64(blocked.Seconds()))
		if blocked >= p.e.opts.PurgeStallAlarm && !stall.told {
			stall.told = true
			p.purgeStalls[name] = stall
			slog.Warn("saga: a durable consumer holds back the partition purge", "partition", p.k, "stream", p.stream,
				"consumer", name, "ack_floor", floor, "blocked", blocked)
		}
	}
	for name := range p.purgeStalls {
		if _, ok := floors[name]; !ok {
			delete(p.purgeStalls, name)
		}
	}
}
