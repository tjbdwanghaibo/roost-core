package nest

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

// preparedEntity 是 I/O 阶段对一个声明目标的读取结果（实体或错误）。
type preparedEntity struct {
	value entity.IThreadSafeEntity
	err   error
}

// preparedGetter 只缓存本次声明目标；加载不获取 Entity 业务锁。
// Touch 保护 I/O → 业务池交接期间的存活；UnTouch 可能触发销毁，因此必须在业务池归还。
type preparedGetter struct {
	base    entity.Getter
	entries map[int64]preparedEntity
	touched []entity.IThreadSafeEntity
}

// Get 优先返回 I/O 阶段准备的结果，未准备的目标交给底层 Getter。
func (p *preparedGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if entry, ok := p.entries[id]; ok {
		return entry.value, entry.err
	}
	return p.base.Get(ctx, id, category)
}

// GetMany 逐个调用 Get，任一失败即返回错误。
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

// add 记录一个目标的准备结果；实体 Touch 成功才缓存并登记待归还的引用，Touch 失败（已摘除）记为 nil。
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

// release 归还准备阶段取得的全部引用。必须在业务池调用（UnTouch 可能触发销毁）。
func (p *preparedGetter) release() {
	values := p.touched
	p.touched = nil
	// 一个销毁 hook panic 不能跳过其余引用的归还。
	for _, value := range values {
		defer value.UnTouch()
	}
}

// prepareIOEntities 在 I/O 阶段加载消息的全部声明目标并 Touch 持有，把 msg.getter 换成只读这些结果的
// preparedGetter；引用由派发收尾经本地执行器在业务池归还。
func prepareIOEntities(msg *Msg) error {
	ids := dispatchIDs(msg)
	p := &preparedGetter{base: msg.getter, entries: make(map[int64]preparedEntity, len(ids))}
	msg.getter = p
	// 即使 getter panic，已取得的引用仍由 dispatch 的业务阶段兜底归还。
	msg.prepared = p
	if msg.Type == MsgTypeMulti || msg.Type == MsgTypeMultiGroup {
		full, categories, err := normalizeFullIDs(ids)
		if err != nil {
			return err
		}
		values, err := p.base.GetMany(nestBaseContext(), full, categories)
		if err != nil {
			return targetLoadError(err)
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

// dispatchGetter 返回当前派发使用的 Getter（I/O 准备后的 preparedGetter 或只读已加载实体的包装），不在派发中时
// 用引擎的 Getter。
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
// 它只在业务池首跑、业务执行之前产生；dispatchNest 据此让派发队列把同一条已准入请求原位
// 转到 I/O 池准备，保留准入资格和同 ID 顺序位置。I/O 准备后的业务续行不再
// 产生它，冷目标错误照常返回。
var errDeclaredTargetCold = errors.New("nest: declared target is not loaded; moving to IO preparation")

// declaredTargetsNeedIOPreparation 在统一准入时判断声明目标里是否有需要加载的冷实体。
// 只用 Getter 可选实现的 entity.LoadedChecker（ManagerAccess 实现为一次内存查找），不调用 Get：
// 准入在发送方 goroutine 上执行——包括在 handler 里发送的业务 worker 和唯一的延迟派发 goroutine——
// 不能依赖自定义 Getter 遵守 LoadedEntitiesOnly。Getter 未实现时 NewEngine 不安装本判定。
// 广播是尽力扇出，冷目标逐个报告（TestShortBroadcastReportsColdTargetAndContinues），不为它批量冷加载；
// 组迁移等内部消息没有业务目标。ID 格式错误留给派发阶段按原语义报告。
func (mgr *NestMgr) declaredTargetsNeedIOPreparation(msg *Msg) bool {
	checker := mgr.loadedChecker
	if checker == nil {
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
		return !checker.IsLoaded(entity.ResolveEntityID(fullID).FullID)
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

// beforeIOPreparation 报告当前派发是否是业务池首跑（尚未经过 I/O 准备，msg.prepared == nil），并且本引擎
// 在准入时做了冷目标判定（Getter 实现 LoadedChecker）。只有这时发现的声明目标冷缺失/驱逐才改写为
// errDeclaredTargetCold；I/O 准备后的续行已持有准备阶段的引用，冷目标错误原样返回，不会第二次迁移。
// 准入不判冷的 Getter 没有“准入时已加载”这个前提，冷目标错误同样原样返回。
func (mgr *NestMgr) beforeIOPreparation() bool {
	if mgr.loadedChecker == nil {
		return false
	}
	msg := currentNestDispatchMsg()
	return msg != nil && msg.prepared == nil
}

// declaredTargetCold 保留原错误，errors.Is 仍可判断 ErrColdLoadInLogic / ErrEntityNotFound / ErrLockTimeout。
func declaredTargetCold(cause error) error {
	return fmt.Errorf("%w: %w", errDeclaredTargetCold, cause)
}

// targetLoadError 把“权威确认声明目标不存在”（AggregateLoader 报告的 entity.ErrAuthorityEntityNotFound，
// DataEngine 的 ErrEntityAggregateNotFound 满足它）包装成稳定的客户端码 errcode.CodeEntityNotFound：首登前的
// 业务请求是可预期的客户端状态，生成 endpoint 经 errcode.ClientError 给客户端该码、不按 CodeInternal 记 ERROR。
// 原错误保留在链上，errors.Is 照旧判别；链上已有业务码时不改写。只在声明目标的加载结果上调用：
// handler 内业务自己返回的错误由业务决定码。
func targetLoadError(err error) error {
	if err == nil || !errors.Is(err, entity.ErrAuthorityEntityNotFound) {
		return err
	}
	var coded *errcode.IntError
	if errors.As(err, &coded) {
		return err
	}
	return errcode.Wrap(errcode.ErrEntityNotFound, err)
}
