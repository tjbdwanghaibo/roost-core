package nest

import (
	"context"
	"errors"
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

// loadedGetter 只在实际访问实体时标记加载约束，保留业务请求的原始 context 身份。
// preparedGetter 缺失的动态目标同样经过此约束，不会持锁退回冷加载。
type loadedGetter struct{ entity.Getter }

func (g loadedGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.Getter.Get(entity.WithLoadedEntitiesOnly(ctx), id, category)
}
func (g loadedGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return g.Getter.GetMany(entity.WithLoadedEntitiesOnly(ctx), ids, categories)
}

// errDeclaredTargetCold 标记“声明目标在 handler 取得 Guard 之前被发现未加载或已被驱逐”。
// 它只在快池首跑、业务执行之前产生；dispatchNest 据此让派发队列把同一条已准入请求原位
// 转到慢池准备，保留准入资格和同 ID 顺序位置（RR-20260926-25）。慢准备后的快续行不再
// 产生它，冷目标错误照常返回。
var errDeclaredTargetCold = errors.New("nest: declared target is not loaded; moving to slow preparation")

// admissionProbeContext 是不可变的共享 ctx，准入判定每条消息都用，避免逐次分配。
var admissionProbeContext = entity.WithLoadedEntitiesOnly(context.Background())

// declaredTargetsNeedSlowPreparation 在统一准入时只读内存判断声明目标里是否有冷实体：
// 按 Getter 的 LoadedEntitiesOnly 契约，未加载且可由 loader 加载的目标返回
// ErrColdLoadInLogic，不做 I/O、不等待加载，所以在调用方 goroutine（包括快 worker）上执行是安全的。
// 广播是尽力扇出，冷目标逐个报告（TestFastBroadcastReportsColdTargetAndContinues），不为它批量冷加载；
// 组迁移等内部消息没有业务目标。ID 格式错误留给派发阶段按原语义报告。
func (mgr *NestMgr) declaredTargetsNeedSlowPreparation(msg *Msg) bool {
	if mgr.getter == nil {
		return false
	}
	switch msg.Type {
	case MsgTypeSingle, MsgTypeMulti, MsgTypeMultiGroup:
	default:
		return false
	}
	cold := func(id int64) bool {
		fullID, err := entity.NormalizeFullID(id, entity.EntityKindNone)
		if err != nil {
			return false
		}
		meta := entity.ResolveEntityID(fullID)
		_, err = mgr.getter.Get(admissionProbeContext, meta.FullID, meta.Category)
		return errors.Is(err, entity.ErrColdLoadInLogic)
	}
	if msg.Tid != 0 && cold(msg.Tid) {
		return true
	}
	for _, id := range msg.Tids {
		if cold(id) {
			return true
		}
	}
	for _, group := range msg.GroupTIds {
		for _, id := range group {
			if cold(id) {
				return true
			}
		}
	}
	return false
}

// beforeSlowPreparation 报告当前派发是否是快池首跑（尚未经过慢准备，msg.prepared == nil）。
// 只有这时发现的声明目标冷缺失/驱逐才改写为 errDeclaredTargetCold；慢准备后的续行已持有
// 准备阶段的引用，冷目标错误原样返回，不会第二次迁移。
func beforeSlowPreparation() bool {
	msg := currentNestDispatchMsg()
	return msg != nil && msg.prepared == nil
}

// declaredTargetCold 保留原错误，errors.Is 仍可判断 ErrColdLoadInLogic / ErrEntityNotFound / ErrLockTimeout。
func declaredTargetCold(cause error) error {
	return fmt.Errorf("%w: %w", errDeclaredTargetCold, cause)
}
