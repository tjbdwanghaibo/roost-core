package policy

import (
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// QueueSpatial 在同一提交事实中移动实体并替换额外覆盖；空 blocks 表示清空。
// 覆盖校验、集合复制在入队前完成，失败不产生部分事实。handler 必须传播错误。
func (in *Interest) QueueSpatial(e entity.IThreadSafeEntity, at spatial.Point, observer bool, blocks []int64) error {
	coverage, err := in.aoi.checkedBlocks(blocks)
	if err != nil {
		return err
	}
	return in.queueFact(e, queuedInterestFact{at: at, observer: observer, coverage: coverage, replaceCoverage: true})
}

// QueueObservedBlocks 只改变被观察覆盖，不改变实体实际位置。
func (in *Interest) QueueObservedBlocks(e entity.IThreadSafeEntity, blocks []int64) error {
	coverage, err := in.aoi.checkedBlocks(blocks)
	if err != nil {
		return err
	}
	return in.queueFact(e, queuedInterestFact{coverage: coverage, replaceCoverage: true, coverageOnly: true})
}

// BlockAt 返回本 Interest 的块编号；块编号不能跨 Interest 混用。
func (in *Interest) BlockAt(at spatial.Point) int64 { return in.aoi.BlockAt(at) }

// ObservedBlocks 返回实体额外覆盖的副本，不包含由位置推导的唯一归属块。
func (in *Interest) ObservedBlocks(id int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]int64(nil), in.aoi.observed[id]...)
}

var ErrSpatialScope = errors.New("policy: spatial component requires a Nest sync mutation scope")

// SpatialComponent 是手写接入件，不保存第二份位置，不要求修改实体生成器。
// writePosition 只在调用者持 Guard 的业务线程上调用，应写入可回滚的 DAO setter；
// 不得执行 I/O 或再次派发业务。所有方法由持有 owner 的 Nest handler 调用。
// 不自动拦截绕过本组件的 DAO 写入，业务移动入口应统一使用 Move/MoveWithObservedBlocks。
type SpatialComponent struct {
	entity.ComponentBase
	owner         entity.IThreadSafeEntity
	interest      *Interest
	observer      bool
	writePosition func(spatial.Point)
}

func NewSpatialComponent(owner entity.IThreadSafeEntity, interest *Interest, observer bool, writePosition func(spatial.Point)) (*SpatialComponent, error) {
	if owner == nil || owner.Base() == nil || interest == nil || writePosition == nil {
		return nil, errors.New("policy: spatial owner, interest and position writer required")
	}
	return &SpatialComponent{owner: owner, interest: interest, observer: observer, writePosition: writePosition}, nil
}
func (*SpatialComponent) Name() string { return "spatial" }

// Move 先完成事实准入，再修改 DAO，避免队列已满时写入位置。
// 事实仍由提交门保护，不会因先入队而提前发布。失败必须由 handler 返回。
func (c *SpatialComponent) Move(to spatial.Point) error {
	if !c.inScope() {
		return ErrSpatialScope
	}
	if err := c.interest.QueueMove(c.owner, to, c.observer); err != nil {
		return err
	}
	c.writePosition(to)
	return nil
}
func (c *SpatialComponent) MoveWithObservedBlocks(to spatial.Point, blocks []int64) error {
	if !c.inScope() {
		return ErrSpatialScope
	}
	if err := c.interest.QueueSpatial(c.owner, to, c.observer, blocks); err != nil {
		return err
	}
	c.writePosition(to)
	return nil
}
func (c *SpatialComponent) SetObservedBlocks(blocks []int64) error {
	if !c.inScope() {
		return ErrSpatialScope
	}
	return c.interest.QueueObservedBlocks(c.owner, blocks)
}

func (c *SpatialComponent) inScope() bool {
	scope := entity.CurrentGuardScope()
	return scope != nil && scope.Guard() != nil && scope.Guard().GuardedEntity(c.owner) && entity.CurrentSyncMutation() != nil
}
