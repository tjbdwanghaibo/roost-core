package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// InspectPartitions 离线只读地重放布局的全部分区（读快照与尾巴事件，与所有者启动时同一个重放 replayOnce），返回全部记录
// （按更新时间、ID 排序）与还没判定的输入条数（启动意图、结果、运维意图还在输入里、水位之后）。合服前检查用：“全部 saga
// 终态” = 没有非终态记录、且没有待判定输入（启动意图还在输入里时 saga 还没建立）。
//
// 不处理输入、不写事件、不做快照、不清理；协调器在跑时结果只是某一时刻的视图（合服按运维手册先停协调器）。layout 与
// replicas 取协调器的同一份配置：快照桶按它打开（桶不存在时会按这份配置建出空桶）。从来没有协调器启动过（没有布局登记）
// 时返回空结果；登记的布局与 layout 不同返回 ErrLayoutMismatch。
func InspectPartitions(ctx context.Context, log fnats.JetStreamLog, layout Layout, replicas int) ([]Record, int, error) {
	if log == nil {
		return nil, 0, errors.New("saga: JetStream log is required")
	}
	if err := layout.Validate(); err != nil {
		return nil, 0, err
	}
	if replicas <= 0 {
		replicas = 1
	}
	snapshots, err := log.KeyValue(ctx, fnats.LogKVConfig{Bucket: layout.SnapshotBucket(), Replicas: replicas, History: 1})
	if err != nil {
		return nil, 0, fmt.Errorf("saga: open snapshot bucket: %w", err)
	}
	entry, err := snapshots.Get(ctx, layoutKey)
	switch {
	case errors.Is(err, fnats.ErrLogNotFound):
		return nil, 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("saga: read registered layout: %w", err)
	}
	var registered layoutRecord
	if json.Unmarshal(entry.Value, &registered) != nil || registered.V != WireVersion || registered.Layout != layoutOf(layout) {
		return nil, 0, fmt.Errorf("%w: registered %s, inspected %+v", ErrLayoutMismatch, entry.Value, layoutOf(layout))
	}
	objects, err := log.Objects(ctx, fnats.LogObjectConfig{Bucket: layout.ObjectBucket(), Replicas: replicas})
	if err != nil {
		return nil, 0, fmt.Errorf("saga: open snapshot objects: %w", err)
	}
	e := &Engine{log: log, layout: layout, now: time.Now, snapshots: snapshots, objects: objects}
	var records []Record
	pending := 0
	for k := range layout.Partitions {
		p := &partition{e: e, k: k, stream: layout.StreamOf(k)}
		stable := false
		for attempt := 0; !stable; attempt++ {
			if attempt > 5 {
				return nil, 0, fmt.Errorf("saga: partition %d kept changing during inspection (is a coordinator still running?)", k)
			}
			if stable, err = p.replayOnce(ctx); err != nil {
				return nil, 0, err
			}
		}
		for _, st := range p.sagas {
			records = append(records, st.Record.Clone())
		}
		pending += len(p.pending)
	}
	sortRecords(records)
	return records, pending, nil
}
