package nest

import (
	"fmt"
	"maps"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/hotcode"
)

// 注册表负责业务入口与事务策略；加载实体、加锁和提交由调度执行路径负责。
var (
	handlerMu  sync.RWMutex
	handlerMap = make(map[HandlerName]handlerEntry)
)

// handlerEntry 是一个已注册 handler 与它归一化后的事务声明。
type handlerEntry struct {
	handler BaseHandler
	meta    HandlerMeta
}

// HandlerPatchName 返回 handler 在 hotcode 中的补丁名。
func HandlerPatchName(name HandlerName) string {
	return "nest.handler." + name.String()
}

// RegisterHandler 以默认事务声明（rollback=state、durability=async）注册包级 handler。
func RegisterHandler(name HandlerName, handler BaseHandler) error {
	return RegisterHandlerWithMeta(name, handler, HandlerMeta{Rollback: RollbackState, Durability: DurabilityAsync})
}

// RegisterMemoryHandler 显式注册不持久化状态的临时 handler（rollback=none、durability=memory）。
// 生产业务修改应使用 RegisterHandler 或 RegisterHandlerWithMeta。
func RegisterMemoryHandler(name HandlerName, handler BaseHandler) error {
	return RegisterHandlerWithMeta(name, handler, HandlerMeta{})
}

// validateHandlerMeta 校验并归一化注册的事务策略。Rollback 不为 none 时必须显式写
// Durability；Rollback 为 none 时未声明归一为 memory，
// 注册表里存的 meta 一律带明确的 durability，执行路径不再看到未声明。
func validateHandlerMeta(name HandlerName, meta HandlerMeta) (HandlerMeta, error) {
	if meta.Rollback > RollbackUndo {
		return meta, fmt.Errorf("nest: invalid rollback policy %d", meta.Rollback)
	}
	if meta.Durability > DurabilityPipelined {
		return meta, fmt.Errorf("nest: invalid durability policy %d", meta.Durability)
	}
	if meta.Durability == durabilityUnset {
		if meta.Rollback != RollbackNone {
			return meta, fmt.Errorf("%w: handler %q declares rollback=%s but no Durability; HandlerMeta.Durability must be set explicitly to one of %s",
				ErrDurabilityUnset, name.String(), meta.Rollback, durabilityChoices)
		}
		meta.Durability = DurabilityMemory
	}
	if meta.Durability != DurabilityMemory && meta.Rollback == RollbackNone {
		return meta, fmt.Errorf("%w: durable handler %q requires rollback", ErrRollbackUnsupported, name.String())
	}
	return meta, nil
}

// RegisterHandlerWithMeta 以显式事务声明注册包级 handler（同时登记到 hotcode）；重名返回错误。
func RegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) error {
	meta, err := validateHandlerMeta(name, meta)
	if err != nil {
		return err
	}
	handlerMu.Lock()
	defer handlerMu.Unlock()
	if _, ok := handlerMap[name]; ok {
		return fmt.Errorf("nest: duplicate handler %q", name.String())
	}
	if err := hotcode.Register(HandlerPatchName(name), handler); err != nil {
		return err
	}
	handlerMap[name] = handlerEntry{handler: handler, meta: meta}
	return nil
}

// MustRegisterHandler 是 RegisterHandler 的 panic 版本。
func MustRegisterHandler(name HandlerName, handler BaseHandler) {
	if err := RegisterHandler(name, handler); err != nil {
		panic(err)
	}
}

// MustRegisterMemoryHandler 是 RegisterMemoryHandler 的 panic 版本。
func MustRegisterMemoryHandler(name HandlerName, handler BaseHandler) {
	if err := RegisterMemoryHandler(name, handler); err != nil {
		panic(err)
	}
}

// MustRegisterHandlerWithMeta 是 RegisterHandlerWithMeta 的 panic 版本。
func MustRegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) {
	if err := RegisterHandlerWithMeta(name, handler, meta); err != nil {
		panic(err)
	}
}

// GetHandler 返回包级注册表中的 handler（已应用 hotcode 补丁）；未注册时为 nil。
func GetHandler(name HandlerName) BaseHandler {
	entry, ok := GetHandlerEntry(name)
	if !ok {
		return nil
	}
	return entry.handler
}

// GetHandlerEntry 返回包级注册表中的条目，handler 已应用 hotcode 补丁。
func GetHandlerEntry(name HandlerName) (handlerEntry, bool) {
	handlerMu.RLock()
	defer handlerMu.RUnlock()
	entry, ok := handlerMap[name]
	if ok && entry.handler != nil {
		entry.handler = hotcode.Resolve[BaseHandler](HandlerPatchName(name), entry.handler)
	}
	return entry, ok
}

// snapshotHandlerEntries 复制包级注册表，作为新引擎的实例注册表初值。
func snapshotHandlerEntries() map[HandlerName]handlerEntry {
	handlerMu.RLock()
	defer handlerMu.RUnlock()
	return maps.Clone(handlerMap)
}

// getHandlerEntry 先查实例注册表，再查包级注册表；handler 已应用 hotcode 补丁。
func (mgr *NestMgr) getHandlerEntry(name HandlerName) (handlerEntry, bool) {
	if mgr == nil {
		return handlerEntry{}, false
	}
	entry, ok := mgr.handlers[name]
	if !ok {
		return GetHandlerEntry(name)
	}
	if entry.handler != nil {
		entry.handler = hotcode.Resolve[BaseHandler](HandlerPatchName(name), entry.handler)
	}
	return entry, ok
}

// RegisterHandlerWithMeta 只在本引擎实例上注册 handler。测试与多引擎进程应优先使用它而不是包级注册表：实例表在
// 引擎之间、`go test -count>1` 重跑之间互不冲突，也不需要 ResetHandlersForTest 清理。只能在 Start 之前注册——
// 派发开始后实例表无锁读取。查找顺序是先实例、后包级（见 getHandlerEntry）。
func (mgr *NestMgr) RegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) error {
	if mgr == nil {
		return fmt.Errorf("nest: nil engine")
	}
	meta, err := validateHandlerMeta(name, meta)
	if err != nil {
		return err
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	if mgr.started {
		return fmt.Errorf("nest: handler %q registered after engine start", name.String())
	}
	if _, ok := mgr.handlers[name]; ok {
		return fmt.Errorf("nest: duplicate handler %q", name.String())
	}
	mgr.handlers[name] = handlerEntry{handler: handler, meta: meta}
	return nil
}

// MustRegisterHandlerWithMeta 是实例级 RegisterHandlerWithMeta 的 panic 版本。
func (mgr *NestMgr) MustRegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) {
	if err := mgr.RegisterHandlerWithMeta(name, handler, meta); err != nil {
		panic(err)
	}
}

// ResetHandlersForTest 清空包级注册表与 hotcode 登记，只供测试使用。
func ResetHandlersForTest() {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	handlerMap = make(map[HandlerName]handlerEntry)
	hotcode.ResetForTest()
}

// HandlerOptionParam 是派发交给 handler 的附加信息：分组消息的每组长度。
type HandlerOptionParam struct {
	IsGroup  bool
	GroupLen []int
}

// HandlerOption 设置 HandlerOptionParam。
type HandlerOption func(opt *HandlerOptionParam)

var (
	// HandlerOptionWithGroup 标记分组消息并复制每组长度。
	HandlerOptionWithGroup = func(groupLen []int) HandlerOption {
		return func(opt *HandlerOptionParam) {
			opt.IsGroup = true
			opt.GroupLen = make([]int, len(groupLen))
			copy(opt.GroupLen, groupLen)
		}
	}
)

// BaseHandler 是生成代码适配后的统一 handler 签名：es 为已加锁的声明目标（按声明顺序，可选目标可为 nil），
// param 为消息参数。
type BaseHandler func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error)
