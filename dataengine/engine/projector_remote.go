package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// remoteProjectionWindow 只组合相邻且不共享 Entity 的纯 Remote 事务。
// RemoteCommit.Validate 保证其 DAO、删除与快照都属于该 Entity；特殊事务是顺序边界。
func remoteProjectionWindow(segments []projectionSegment, workers, maxBytes int) int {
	if workers <= 1 {
		return 0
	}
	entities := make(map[int64]struct{})
	transactions := make(map[coredata.TransactionID]struct{})
	size, count := 0, 0
	for _, segment := range segments {
		if count == workers || len(segment.records) != 1 {
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

// 每条记录独立完成；checkpoint 只能跨过连续成功前缀。
// 失败之后已提交的后缀仍留在 WAL，下次通过 Mongo 的持久事务身份重放。
// 返回前必须等所有工作结束，禁止后台写跨越下一窗口或 Shutdown。
func (projector *Projector) projectRemoteWindow(ctx context.Context, segments []projectionSegment) (int, error) {
	errs := make([]error, len(segments))
	var workers sync.WaitGroup
	for i, segment := range segments {
		workers.Go(func() {
			record := segment.records[0]
			if err := ctx.Err(); err != nil {
				errs[i] = err
				return
			}
			errs[i] = projector.store.Project(ctx, record)
			if errs[i] == nil {
				projector.completeProjection(record.ID, nil)
				projector.projected.Add(1)
			}
		})
	}
	workers.Wait()
	prefix := 0
	for prefix < len(errs) && errs[prefix] == nil {
		prefix++
	}
	if prefix == len(errs) {
		return prefix, nil
	}
	return prefix, fmt.Errorf("dataengine projector: remote transaction %s: %w", segments[prefix].records[0].ID.String(), errors.Join(errs...))
}
