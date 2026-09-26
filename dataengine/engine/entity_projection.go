package engine

import (
	"context"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// 只保留本进程在途投影；启动恢复在 Runtime.Ready 前排空历史 WAL。
// DAO 共用完整 Entity ID，因此重载只等相关实体，不阻塞于全局冷数据或无关事务。
type entityProjection struct {
	ids  []int64
	done chan struct{}
	err  error
}

func (p *Projector) trackEntitiesLocked(record coredata.CommitRecord) {
	if p.pendingTransactions == nil {
		p.pendingTransactions = make(map[coredata.TransactionID]*entityProjection)
		p.pendingEntities = make(map[int64]map[coredata.TransactionID]*entityProjection)
	}
	pending := &entityProjection{done: make(chan struct{})}
	for _, mutation := range record.Mutations {
		id := mutation.Key.ID
		if id == 0 {
			id = mutation.EntityID
		}
		if id == 0 {
			continue
		}
		group := p.pendingEntities[id]
		if group == nil {
			group = make(map[coredata.TransactionID]*entityProjection)
			p.pendingEntities[id] = group
		}
		if group[record.ID] != nil {
			continue
		}
		group[record.ID] = pending
		pending.ids = append(pending.ids, id)
	}
	p.pendingTransactions[record.ID] = pending
}
func (p *Projector) finishEntities(id coredata.TransactionID, err error) {
	p.heldMu.Lock()
	defer p.heldMu.Unlock()
	p.finishEntitiesLocked(id, err)
}
func (p *Projector) finishEntitiesLocked(id coredata.TransactionID, err error) {
	pending := p.pendingTransactions[id]
	if pending == nil {
		return
	}
	for _, entityID := range pending.ids {
		group := p.pendingEntities[entityID]
		delete(group, id)
		if len(group) == 0 {
			delete(p.pendingEntities, entityID)
		}
	}
	delete(p.pendingTransactions, id)
	pending.err = err
	close(pending.done)
}

// WaitEntityProjection 等待实体 id 在本进程已准入的全部投影完成。等待前、等待中、等待后
// 都能感知“本进程不会再投影”：投影 fatal（ErrProjectionConflict 等，可 errors.Is 判别）、
// Projector 已关闭（ErrRuntimeStopped）、WAL 不健康。fatal 会唤醒全部等待方，不只 fatal 批次。
func (p *Projector) WaitEntityProjection(ctx context.Context, id int64) error {
	fctx.AssertBlockingAllowed("dataengine.WaitEntityProjection")
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.projectionUsable(); err != nil {
		return err
	}
	p.heldMu.RLock()
	pending := make([]*entityProjection, 0, len(p.pendingEntities[id]))
	for _, item := range p.pendingEntities[id] {
		pending = append(pending, item)
	}
	p.heldMu.RUnlock()
	for _, item := range pending {
		select {
		case <-item.done:
			if item.err != nil {
				return item.err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-p.ctx.Done():
			return ErrRuntimeStopped
		}
	}
	return p.projectionUsable()
}

// projectionUsable 报告冷读是否还能相信“等到的投影就是全部”：fatal 或关闭后，
// 已准入但未投影的记录不会再被本进程投影，WAL 不健康时准入结果本身不确定。
func (p *Projector) projectionUsable() error {
	if fatal := p.fatal(); fatal != nil {
		return fatal
	}
	if p.ctx.Err() != nil {
		return ErrRuntimeStopped
	}
	return p.wal.Healthy()
}
