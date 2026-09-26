package engine

import (
	"context"
	"errors"
	"fmt"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// remoteProjectionWindow 只组合相邻且不共享 Entity 的纯 Remote 事务。
// RemoteCommit.Validate 保证其 DAO、删除与快照都属于该 Entity；特殊事务是顺序边界。
func remoteProjectionWindow(segments []projectionSegment, maxRecords, maxBytes int) int {
	if maxRecords <= 1 {
		return 0
	}
	entities := make(map[int64]struct{})
	transactions := make(map[coredata.TransactionID]struct{})
	size, count := 0, 0
	for _, segment := range segments {
		if count == maxRecords || len(segment.records) != 1 {
			break
		}
		record := segment.records[0]
		if record.Handler == MigrationHandler || len(record.Mutations) == 0 || len(record.Effects) != 0 || len(record.Receipts) != 0 {
			break
		}
		if _, exists := transactions[record.ID]; exists {
			break
		}
		valid := true
		for _, mutation := range record.Mutations {
			c := mutation.Remote
			if c == nil || c.TransactionID != entity.RemoteTransactionID(record.ID) || c.Validate() != nil {
				valid = false
				break
			}
			if _, exists := entities[c.EntityID]; exists {
				valid = false
				break
			}
		}
		if !valid {
			break
		}
		bytes := projectionRecordLogicalBytes(record)
		if count > 0 && size > maxBytes-bytes {
			break
		}
		for _, mutation := range record.Mutations {
			entities[mutation.Remote.EntityID] = struct{}{}
		}
		transactions[record.ID] = struct{}{}
		size = saturatingAdd(size, bytes)
		count++
	}
	return count
}

// 每条记录独立完成；空闲 worker 可继续处理本窗口的下一条独立记录。
// 窗口受回放记录/字节预算限制，并在共享 Entity、事务身份或特殊内容之前截止。
// 首次观察到失败后停止补位，等待已经发出的工作全部返回；只确认连续成功前缀。
func (projector *Projector) projectRemoteWindow(ctx context.Context, segments []projectionSegment) (int, error) {
	type result struct {
		index int
		err   error
	}
	concurrency := min(projector.opts.RemoteProjectionWorkers, len(segments))
	completed := make(chan result, concurrency)
	errs := make([]error, len(segments))
	next, active := 0, 0
	failed := false
	launch := func() {
		index := next
		next++
		active++
		go func() {
			record := segments[index].records[0]
			err := ctx.Err()
			if err == nil {
				err = projector.store.Project(ctx, record)
			}
			if err == nil {
				projector.completeProjection(record.ID, nil)
				projector.projected.Add(1)
			} else {
				err = fmt.Errorf("remote transaction %v: %w", record.ID, err)
			}
			completed <- result{index, err}
		}()
	}
	for next < concurrency {
		launch()
	}
	for active > 0 {
		done := <-completed
		active--
		errs[done.index] = done.err
		failed = failed || done.err != nil
		if !failed && ctx.Err() == nil && next < len(segments) {
			launch()
		}
	}
	prefix := 0
	for prefix < next && errs[prefix] == nil {
		prefix++
	}
	if prefix == len(segments) {
		return prefix, nil
	}
	cause := errors.Join(append(errs[:next], ctx.Err())...)
	return prefix, fmt.Errorf("dataengine projector: remote window stopped after %d/%d records: %w", prefix, len(segments), cause)
}
