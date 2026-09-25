package nest

import (
	"fmt"
	"maps"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/hotcode"
)

// 注册表负责业务入口与事务策略；加载实体、加锁和提交由调度执行路径负责。
var (
	handlerMu  sync.RWMutex
	handlerMap = make(map[HandlerName]handlerEntry)
)

type handlerEntry struct {
	handler BaseHandler
	meta    HandlerMeta
}

func HandlerPatchName(name HandlerName) string {
	return "nest.handler." + name.String()
}

func RegisterHandler(name HandlerName, handler BaseHandler) error {
	return RegisterHandlerWithMeta(name, handler, HandlerMeta{Rollback: RollbackState, Durability: DurabilityAsync})
}

// RegisterMemoryHandler is the explicit opt-out for ephemeral handlers whose
// state must never be persisted. Production business mutations should use
// RegisterHandler or RegisterHandlerWithMeta.
func RegisterMemoryHandler(name HandlerName, handler BaseHandler) error {
	return RegisterHandlerWithMeta(name, handler, HandlerMeta{})
}

func validateHandlerMeta(name HandlerName, meta HandlerMeta) error {
	if meta.Rollback > RollbackUndo {
		return fmt.Errorf("nest: invalid rollback policy %d", meta.Rollback)
	}
	if meta.Durability > DurabilityPipelined {
		return fmt.Errorf("nest: invalid durability policy %d", meta.Durability)
	}
	if meta.Durability != DurabilityMemory && meta.Rollback == RollbackNone {
		return fmt.Errorf("%w: durable handler %q requires rollback", ErrRollbackUnsupported, name.String())
	}
	return nil
}

func RegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) error {
	if err := validateHandlerMeta(name, meta); err != nil {
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

func MustRegisterHandler(name HandlerName, handler BaseHandler) {
	if err := RegisterHandler(name, handler); err != nil {
		panic(err)
	}
}

func MustRegisterMemoryHandler(name HandlerName, handler BaseHandler) {
	if err := RegisterMemoryHandler(name, handler); err != nil {
		panic(err)
	}
}

func MustRegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) {
	if err := RegisterHandlerWithMeta(name, handler, meta); err != nil {
		panic(err)
	}
}

func GetHandler(name HandlerName) BaseHandler {
	entry, ok := GetHandlerEntry(name)
	if !ok {
		return nil
	}
	return entry.handler
}

func GetHandlerEntry(name HandlerName) (handlerEntry, bool) {
	handlerMu.RLock()
	defer handlerMu.RUnlock()
	entry, ok := handlerMap[name]
	if ok && entry.handler != nil {
		entry.handler = hotcode.Resolve[BaseHandler](HandlerPatchName(name), entry.handler)
	}
	return entry, ok
}

func snapshotHandlerEntries() map[HandlerName]handlerEntry {
	handlerMu.RLock()
	defer handlerMu.RUnlock()
	return maps.Clone(handlerMap)
}

func (mgr *NestMgr) getHandlerEntry(name HandlerName) (handlerEntry, bool) {
	if mgr == nil {
		return handlerEntry{}, false
	}
	entry, ok := mgr.handlers[name]
	if !ok {
		return GetHandlerEntry(name)
	}
	if ok && entry.handler != nil {
		entry.handler = hotcode.Resolve[BaseHandler](HandlerPatchName(name), entry.handler)
	}
	return entry, ok
}

// RegisterHandlerWithMeta registers a handler on this engine instance only.
// Tests and multi-engine processes should prefer it over the package-global
// registry: instance tables don't collide across engines or across
// `go test -count>1` reruns, and need no ResetHandlersForTest hygiene.
// Instance registration is only valid before Start — the table is read
// lock-free once dispatch begins. Lookup order is instance first, then the
// global registry (see getHandlerEntry).
func (mgr *NestMgr) RegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) error {
	if mgr == nil {
		return fmt.Errorf("nest: nil engine")
	}
	if err := validateHandlerMeta(name, meta); err != nil {
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

// MustRegisterHandlerWithMeta is the panicking form of the instance-scoped
// RegisterHandlerWithMeta.
func (mgr *NestMgr) MustRegisterHandlerWithMeta(name HandlerName, handler BaseHandler, meta HandlerMeta) {
	if err := mgr.RegisterHandlerWithMeta(name, handler, meta); err != nil {
		panic(err)
	}
}

func ResetHandlersForTest() {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	handlerMap = make(map[HandlerName]handlerEntry)
	hotcode.ResetForTest()
}

type HandlerOptionParam struct {
	IsGroup  bool
	GroupLen []int
}

type HandlerOption func(opt *HandlerOptionParam)

var (
	HandlerOptionWithGroup = func(groupLen []int) HandlerOption {
		return func(opt *HandlerOptionParam) {
			opt.IsGroup = true
			opt.GroupLen = make([]int, len(groupLen))
			copy(opt.GroupLen, groupLen)
		}
	}
)

type BaseHandler func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error)
