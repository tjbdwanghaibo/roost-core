package nest

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type preparedEntity struct {
	value entity.IThreadSafeEntity
	err   error
}

// preparedGetter 只缓存本次声明目标；加载不获取 Entity 业务锁。
// Touch 保护慢→快交接期间的存活；UnTouch 可能触发销毁，因此必须在快池归还。
type preparedGetter struct {
	base    entity.Getter
	entries map[int64]preparedEntity
	touched []entity.IThreadSafeEntity
}

func (p *preparedGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if entry, ok := p.entries[id]; ok {
		return entry.value, entry.err
	}
	return p.base.Get(ctx, id, category)
}
func (p *preparedGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	result := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		var err error
		result[i], err = p.Get(ctx, id, categories[i])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (p *preparedGetter) add(id int64, value entity.IThreadSafeEntity, err error) {
	if value != nil {
		if value.Touch() {
			p.touched = append(p.touched, value)
		} else {
			value = nil
		}
	}
	p.entries[id] = preparedEntity{value, err}
}
func (p *preparedGetter) release() {
	values := p.touched
	p.touched = nil
	// 一个销毁 hook panic 不能跳过其余引用的归还。
	for _, value := range values {
		defer value.UnTouch()
	}

}
func prepareSlowEntities(msg *Msg) error {
	ids := dispatchIDs(msg)
	p := &preparedGetter{base: msg.getter, entries: make(map[int64]preparedEntity, len(ids))}
	msg.getter = p
	// 即使 getter panic，已取得的引用仍由 dispatch 的快阶段兜底归还。
	msg.prepared = p
	if msg.Type == MsgTypeMulti || msg.Type == MsgTypeMultiGroup {
		full, categories, err := normalizeFullIDs(ids)
		if err != nil {
			return err
		}
		values, err := p.base.GetMany(nestBaseContext(), full, categories)
		if err != nil {
			return err
		}
		if len(values) != len(full) {
			return fmt.Errorf("nest: getter returned %d entities for %d targets", len(values), len(full))
		}
		for i, id := range full {
			p.add(id, values[i], nil)
		}
	} else {
		for _, id := range ids {
			meta := entity.ResolveEntityID(id)
			value, err := p.base.Get(nestBaseContext(), id, meta.Category)
			p.add(id, value, err)
		}
	}
	return nil
}
func (mgr *NestMgr) dispatchGetter() entity.Getter {
	if msg := currentNestDispatchMsg(); msg != nil && msg.getter != nil {
		return msg.getter
	}
	return mgr.getter
}
