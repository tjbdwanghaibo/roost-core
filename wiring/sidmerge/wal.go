package sidmerge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// WALReport 是一个已停进程的 WAL 目录是否排空的结论。Drained 只在 WAL 完整、没有被进程持有、且 checkpoint 等于
// 最后一条记录（或根本没有记录）时为 true。game-demo 的 cmd/sidmove wal-drained 与本包的合服检查用同一个判断。
type WALReport struct {
	Dir        string `json:"dir"`
	Drained    bool   `json:"drained"`
	Records    uint64 `json:"records_retained"`
	Checkpoint string `json:"checkpoint,omitempty"`
	LastRecord string `json:"last_record,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Databases 是写下这些记录的进程的逻辑库 → 物理库映射（WAL 目录的 databases.json）：搬机后新进程
	// 的 dataengine.databases 应与它一致，否则同一份数据会去不同的库里找。
	Databases map[string]string `json:"databases,omitempty"`
}

// ErrWALNotDrained 表示 WAL 里还有没投影确认的记录：用空 WAL 目录接管（搬机）或让这个 sid 退役（合服）会丢掉它们。
var ErrWALNotDrained = errors.New("the WAL still holds records that are not acknowledged as projected")

// WALDrained 只读检查一个已停止的 WAL 目录（nestwal.Inspect 先拿 writer.lock，进程还在跑就拒绝），比较最新有效
// checkpoint 与最后一条记录的位置。它不证明 outbox 已排空：停机时已暂存、未发布的 effect 留在 Mongo 的 outbox 里
// （合服检查另查 outbox）。
func WALDrained(ctx context.Context, dir string) (WALReport, error) {
	report := WALReport{Dir: dir}
	if dir == "" {
		return report, errors.New("the WAL directory is required")
	}
	inspection, err := nestwal.Inspect(withContext(ctx), nestwal.InspectOptions{Dir: dir})
	if err != nil {
		report.Reason = err.Error()
		if errors.Is(err, nestwal.ErrLocked) {
			report.Reason = "a process still holds this WAL; stop it first"
		}
		return report, err
	}
	report.Databases = inspection.Databases
	var checkpoint *nestwal.CheckpointInspection
	for i := range inspection.Checkpoints {
		entry := &inspection.Checkpoints[i]
		if entry.Error == "" && (checkpoint == nil || entry.Generation > checkpoint.Generation) {
			checkpoint = entry
		}
	}
	var last *nestwal.SegmentInspection
	for i := range inspection.Segments {
		segment := &inspection.Segments[i]
		report.Records += segment.Records
		if segment.Records > 0 {
			last = segment
		}
	}
	if checkpoint != nil {
		report.Checkpoint = fmt.Sprintf("segment %d offset %d", checkpoint.Fence.Segment, checkpoint.Fence.Offset)
	}
	if last == nil {
		report.Drained = true
		report.Reason = "no records retained"
		return report, nil
	}
	lastSegment, err := segmentNumber(last.Name)
	if err != nil {
		return report, err
	}
	report.LastRecord = fmt.Sprintf("segment %d offset %d (%s)", lastSegment, last.LastGoodOffset, last.LastTransactionID)
	if checkpoint != nil && checkpoint.Fence.Segment == lastSegment && checkpoint.Fence.Offset == last.LastGoodOffset {
		report.Drained = true
		return report, nil
	}
	report.Reason = "the checkpoint is behind the last record"
	return report, ErrWALNotDrained
}

// segmentNumber 读出段文件名 segment-<20 位序号>.wal 里的序号（nestwal 的段命名）。
func segmentNumber(name string) (uint64, error) {
	digits, ok := strings.CutPrefix(strings.TrimSuffix(name, ".wal"), "segment-")
	if !ok {
		return 0, fmt.Errorf("unexpected segment name %q", name)
	}
	return strconv.ParseUint(digits, 10, 64)
}
