package nestwal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// InspectOptions 只用于停机后的诊断。SnapshotDir非空时必须是尚不存在的目录，
// 完整副本放到其中的wal/，附带哈希清单和诊断报告；不修改原WAL或checkpoint。
type InspectOptions struct {
	Dir            string
	SnapshotDir    string
	MaxRecordBytes int
}

type CheckpointInspection struct {
	Name       string               `json:"name"`
	Generation uint64               `json:"generation"`
	Fence      corenest.CommitFence `json:"fence"`
	Error      string               `json:"error,omitempty"`
}

type SegmentInspection struct {
	Name              string `json:"name"`
	Bytes             int64  `json:"bytes"`
	Records           uint64 `json:"records"`
	LastGoodOffset    int64  `json:"last_good_offset"`
	LastTransactionID string `json:"last_transaction_id,omitempty"`
	Error             string `json:"error,omitempty"`
}

type Inspection struct {
	Source      string                 `json:"source"`
	Complete    bool                   `json:"complete"`
	Checkpoints []CheckpointInspection `json:"checkpoints"`
	Segments    []SegmentInspection    `json:"segments"`
	Problems    []string               `json:"problems,omitempty"`
}

// Inspect 取得现有writer.lock后只读检查，不调用会截断尾帧的Open。
// 每段遇首个坏帧即停止，LastGoodOffset也是下一条失败记录的起点；不搜索魔数跳过坏数据。
// 即使返回ErrCorrupt，报告和完整快照仍可用于定位；格式合法不等于权威投影一定成功。
func Inspect(ctx context.Context, opts InspectOptions) (report Inspection, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if opts.Dir == "" {
		return report, errors.New("nestwal: inspection directory is required")
	}
	if opts.MaxRecordBytes < 0 {
		return report, errors.New("nestwal: inspection record limit must not be negative")
	}
	source, err := filepath.EvalSymlinks(opts.Dir)
	if err != nil {
		return report, err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return report, err
	}
	report.Source = source
	// 拒绝目录内链接和特殊文件；快照只包含这个WAL目录拥有的普通文件。
	if err := inspectRegularFiles(source); err != nil {
		return report, err
	}
	guard, err := os.Open(filepath.Join(source, "writer.lock"))
	if err != nil {
		return report, fmt.Errorf("nestwal: inspection needs an existing writer.lock: %w", err)
	}
	defer func() { err = errors.Join(err, guard.Close()) }()
	if err := lockFile(guard); err != nil {
		return report, errors.Join(ErrLocked, err)
	}
	defer func() { err = errors.Join(err, unlockFile(guard)) }()
	var snapshot string
	if opts.SnapshotDir != "" {
		snapshot, err = snapshotWAL(ctx, source, opts.SnapshotDir)
		if err != nil {
			return report, err
		}
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = DefaultOptions(source).MaxRecordBytes
	}
	err = inspectWAL(ctx, source, opts.MaxRecordBytes, &report)
	if snapshot != "" && report.Complete {
		if saveErr := finishWALSnapshot(snapshot, report); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
	}
	return report, err
}

func inspectWAL(ctx context.Context, dir string, maxRecord int, report *Inspection) error {
	var failure error
	validCheckpoints := 0
	for slot := range 2 {
		name := checkpointName(slot)
		state, err := readCheckpoint(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		entry := CheckpointInspection{Name: name, Generation: state.generation, Fence: state.fence}
		if err != nil {
			entry.Error = err.Error()
			// 正常恢复可选另一槽。诊断仍保留坏槽，不能把loadCheckpoint的容错当作两槽都完整。
		} else {
			validCheckpoints++
		}
		report.Checkpoints = append(report.Checkpoints, entry)
	}
	if len(report.Checkpoints) > 0 && validCheckpoints == 0 {
		report.Problems = append(report.Problems, "no valid checkpoint slot; normal recovery would replay from the retained beginning")
		failure = errors.Join(failure, ErrCorrupt)
	}
	segments, err := listSegments(dir)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		report.Problems = append(report.Problems, "no WAL segments found")
		report.Complete = true
		return ErrCorrupt
	}
	checkpoint, _ := loadCheckpoint(dir)
	if segments[0] > 1 && segments[0] > checkpoint.fence.Segment {
		report.Problems = append(report.Problems, "retained WAL begins after the selected checkpoint; required prefix may be missing")
		failure = errors.Join(failure, ErrCorrupt)
	}
	checkpointFound := checkpoint.fence.Segment == 0 && checkpoint.fence.Offset == 0
	for i, number := range segments {
		if err := ctx.Err(); err != nil {
			return errors.Join(failure, err)
		}
		if i > 0 && number != segments[i-1]+1 {
			report.Problems = append(report.Problems, fmt.Sprintf("missing segment between %d and %d", segments[i-1], number))
			failure = errors.Join(failure, ErrCorrupt)
		}
		entry := SegmentInspection{Name: segmentName(number)}
		file, err := os.Open(filepath.Join(dir, entry.Name))
		if err != nil {
			return errors.Join(failure, err)
		}
		info, err := file.Stat()
		if err == nil {
			entry.Bytes = info.Size()
			err = scanFramesFrom(file, number, 0, info.Size(), maxRecord, false, func(fence corenest.CommitFence, payload []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				record, err := decodeRecord(payload)
				if err != nil {
					return errors.Join(ErrCorrupt, err)
				}
				entry.Records++
				entry.LastGoodOffset = fence.Offset
				entry.LastTransactionID = record.ID.String()
				if fence == checkpoint.fence {
					checkpointFound = true
				}
				return nil
			})
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			entry.Error = err.Error()
			failure = errors.Join(failure, fmt.Errorf("%s offset %d: %w", entry.Name, entry.LastGoodOffset, err))
		}
		report.Segments = append(report.Segments, entry)
		if err := ctx.Err(); err != nil {
			return errors.Join(failure, err)
		}
	}
	// 正常清理只删除checkpoint之前的旧段，不应丢失checkpoint所在段。
	if !checkpointFound {
		report.Problems = append(report.Problems, "selected checkpoint does not match a validated record boundary")
		failure = errors.Join(failure, ErrCorrupt)
	}
	report.Complete = true
	return failure
}
