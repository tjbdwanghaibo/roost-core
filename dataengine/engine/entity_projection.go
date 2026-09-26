package engine

import (
	"context"
	"errors"
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
func (p *Projector) WaitEntityProjection(ctx context.Context, id int64) error {
	fctx.AssertBlockingAllowed("dataengine.WaitEntityProjection")
	if ctx == nil {
		ctx = context.Background()
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
	return errors.Join(p.fatal(), p.wal.Healthy())
}
