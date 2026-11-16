package entity

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
)

// 实体事件：同一实体、同一笔事务内的同步通知 / 拦截（方案见 docs/maintenance/FEATURE-2026-11-16-tx-event.md）。
//
// 契约：
//   - 监听只由生成代码在 init 里经 RegisterEventListener 登记；登记表在第一次 Publish 时冻结，之后只读、不加锁。
//   - Publish 只在当前 goroutine 的 Guard 持有本实体时执行。不要求可回滚事务：memory 快路径和 rollback=none 的 handler
//     里也能发布，监听返回的错误照常外传，但已做的修改不撤销。
//   - 事件只在发布它的实体上分发，立即执行、深度优先；不异步、不跨实体、不跨进程（跨实体走 Nest 发送，跨进程走 nest.Emit）。
//   - 监听按登记顺序执行，第一个错误即停；执行顺序（优先级等）由生成器在生成时排好，core 只按登记先后执行。
//   - 循环由三道上限挡住（嵌套深度、同一事件同时在栈上的次数、一次最外层发布内的总次数），超出即返回带完整事件链的错误。

var (
	// ErrEventNotGuarded 表示当前 goroutine 的 Guard 没有持有发布事件的实体。
	ErrEventNotGuarded = errors.New("entity event: entity is not locked by the current guard")
	// ErrEventDispatchMissing 表示实体没有装配事件分发（生成的工厂没有调用 SetEventDispatch）。
	ErrEventDispatchMissing = errors.New("entity event: event dispatch is not installed")
	// ErrEventLimit 表示超出了事件嵌套深度、同一事件重入次数或一次最外层发布的总次数上限。
	ErrEventLimit = errors.New("entity event: limit exceeded")
)

// 事件上限的缺省值。正常业务不应接近这些上限；只有确有需要时在 init 里经 SetEventLimits 调整。
const (
	DefaultEventMaxDepth     = 8
	DefaultEventMaxSameEvent = 3
	DefaultEventMaxPerRoot   = 256
)

// eventLimits 只在 init 期（第一次发布之前）写入，之后只读。
var eventLimits = struct{ depth, sameEvent, perRoot int }{
	depth: DefaultEventMaxDepth, sameEvent: DefaultEventMaxSameEvent, perRoot: DefaultEventMaxPerRoot,
}

// eventKey 是登记表的键：实体种类 + 事件的动态类型。
type eventKey struct {
	kind EntityKind
	typ  reflect.Type
}

// eventListener 是一条已登记的监听：名字用于错误信息与去重，fn 已做好实体与事件的类型断言。
type eventListener struct {
	name string
	fn   func(owner IThreadSafeEntity, ev any) error
}

// eventListeners 只在 init 期写入；eventFrozen 置位（第一次发布）之后只读，所以读路径不加锁。
// 冻结点取首次发布而不是 App 启动，是为了不让 entity 包依赖 app 的生命周期。
var (
	eventListeners = make(map[eventKey][]eventListener)
	eventFrozen    atomic.Bool
)

// freezeEvents 在第一次发布时冻结登记表与上限；之后再登记或调整上限会 panic。
func freezeEvents() {
	if !eventFrozen.Load() {
		eventFrozen.Store(true)
	}
}

// RegisterEventListener 把一个回调登记到 kind 这种实体的事件 E 上，只由生成代码在 init 里调用。
// eventName 用于日志和错误信息，取 E 的类型名；listener 是 "<接收者>.<方法>"，例如 "activity.Component.OnProfileLevelUp"。
// 回调按登记先后执行。同一 (kind, E, listener) 重复登记，或在第一次 Publish 之后登记，直接 panic。
func RegisterEventListener[T IThreadSafeEntity, E any](kind EntityKind, eventName, listener string, fn func(T, E) error) {
	if eventFrozen.Load() {
		panic(fmt.Sprintf("entity event: register %s listener %s after the first Publish", eventName, listener))
	}
	if kind == EntityKindNone {
		panic(fmt.Sprintf("entity event: register %s listener %s with kind none", eventName, listener))
	}
	if listener == "" || fn == nil {
		panic(fmt.Sprintf("entity event: register %s listener requires a name and a callback", eventName))
	}
	typ := reflect.TypeFor[E]()
	if typ.Kind() == reflect.Interface {
		// 分发按事件的动态类型查表，接口类型永远查不到。
		panic(fmt.Sprintf("entity event: %s listener %s: event type %s must be a concrete type", eventName, listener, typ))
	}
	key := eventKey{kind: kind, typ: typ}
	for _, existing := range eventListeners[key] {
		if existing.name == listener {
			panic(fmt.Sprintf("entity event: duplicate listener %s for %s on kind %d", listener, eventName, kind))
		}
	}
	eventListeners[key] = append(eventListeners[key], eventListener{
		name: listener,
		fn: func(owner IThreadSafeEntity, ev any) error {
			typed, ok := owner.(T)
			if !ok {
				return fmt.Errorf("entity event: listener %s expects %s, got %T", listener, reflect.TypeFor[T](), owner)
			}
			return fn(typed, ev.(E))
		},
	})
}

// SetEventLimits 调整事件上限：嵌套深度、同一事件同时在栈上的次数、一次最外层发布内的总次数。只能在 init 里调用；
// 第一次 Publish 之后调用或传入非正数会 panic。
func SetEventLimits(depth, sameEvent, perRoot int) {
	if eventFrozen.Load() {
		panic("entity event: SetEventLimits after the first Publish")
	}
	if depth <= 0 || sameEvent <= 0 || perRoot <= 0 {
		panic(fmt.Sprintf("entity event: limits must be positive (depth=%d same=%d perRoot=%d)", depth, sameEvent, perRoot))
	}
	eventLimits.depth, eventLimits.sameEvent, eventLimits.perRoot = depth, sameEvent, perRoot
}

// eventStack 是一个实体上正在执行的事件链（最外层在前）和本次最外层发布内的发布次数。它只在实体被当前 Guard 锁住时
// 读写，所以不加锁；每次最外层 Publish 返回（包括 panic）时已经清空，不会把状态带进下一笔事务。
type eventStack struct {
	types     []reflect.Type
	rootCount int
}

// SetEventDispatch 装配本实体的事件分发，由生成的实体工厂调用，和 SetHooks 同一套路：EntityBase 不持有外层实体，
// 只存这个函数。生成代码写成 e.SetEventDispatch(func(ev any) error { return entity.DispatchEvent(KindX, e, ev) })。
func (e *EntityBase) SetEventDispatch(dispatch func(ev any) error) {
	e.eventDispatch = dispatch
}

// Publish 在本实体上发布事件 ev：立即按登记顺序执行这种实体对 ev 类型的全部监听，第一个错误即停并返回。
// 当前 Guard 必须持有本实体；不要求可回滚事务（见文件头契约）。监听里再 Publish 会深度优先立即执行，
// 内层错误必须原样返回给外层，否则内层出错前的修改会留下、整笔事务只回滚不到。
// 事件层不 recover：panic 照常抛给 Nest，事件栈由 defer 弹出。
func (e *EntityBase) Publish(ev any) error {
	freezeEvents()
	if e == nil {
		return fmt.Errorf("%w: nil entity", ErrEventNotGuarded)
	}
	if ev == nil {
		return errors.New("entity event: publish nil event")
	}
	typ := reflect.TypeOf(ev)
	if !e.guardedByCurrentScope() {
		return fmt.Errorf("%w: publish %s on entity %d", ErrEventNotGuarded, eventTypeName(typ), e.id)
	}
	dispatch := e.eventDispatch
	if dispatch == nil {
		return fmt.Errorf("%w: publish %s on entity %d (kind %d); the entity factory must call SetEventDispatch",
			ErrEventDispatchMissing, eventTypeName(typ), e.id, e.kind)
	}
	if err := e.pushEvent(typ); err != nil {
		return err
	}
	defer e.popEvent()
	return dispatch(ev)
}

// guardedByCurrentScope 报告当前 goroutine 的 Guard 是否持有本实体的锁：Guard 账本里这个 ID 登记的是本实例，
// 或者是共用同一把锁的另一个实例（与 EntityGuard.holding 同一判据；EntityBase 不持有外层实体，只能按 Base 比较）。
func (e *EntityBase) guardedByCurrentScope() bool {
	if e.id == 0 {
		return false
	}
	guard := CurrentGuardScope().Guard()
	if guard == nil {
		return false
	}
	current, ok := guard.eMap[e.id]
	if !ok || current == nil {
		return false
	}
	return current.Base() == e || sameMutex(current.GetMutex(), e.mu)
}

// pushEvent 检查三道上限后把 typ 压栈。超出上限时不压栈、不执行监听，返回带完整事件链（含这一次）的错误。
func (e *EntityBase) pushEvent(typ reflect.Type) error {
	stack := &e.events
	if len(stack.types) == 0 {
		stack.rootCount = 0
	}
	stack.rootCount++
	limit := eventLimits
	switch {
	case len(stack.types)+1 > limit.depth:
		return fmt.Errorf("%w: %s: nesting depth exceeds %d", ErrEventLimit, eventChain(stack.types, typ), limit.depth)
	case countEvent(stack.types, typ)+1 > limit.sameEvent:
		return fmt.Errorf("%w: %s: %s is on the stack more than %d times", ErrEventLimit, eventChain(stack.types, typ), eventTypeName(typ), limit.sameEvent)
	case stack.rootCount > limit.perRoot:
		return fmt.Errorf("%w: %s: more than %d publishes within one outermost Publish", ErrEventLimit, eventChain(stack.types, typ), limit.perRoot)
	}
	stack.types = append(stack.types, typ)
	return nil
}

// popEvent 弹出栈顶事件；栈空时（最外层发布返回）同时清零本次的发布计数。
func (e *EntityBase) popEvent() {
	stack := &e.events
	if n := len(stack.types); n > 0 {
		stack.types[n-1] = nil
		stack.types = stack.types[:n-1]
	}
	if len(stack.types) == 0 {
		stack.rootCount = 0
	}
}

// EventError 是监听出错时 Publish 返回的错误：Chain 是出错那一刻的完整事件链（"A → B → C"），Listener 是出错的监听，
// Err 是它返回的错误（Unwrap 得到，errors.Is 照常可用）。
//
// 嵌套发布时错误只包一次：内层监听出错，内层 Publish 返回的 EventError 已经带着完整事件链和出错的监听；外层监听把它
// 原样（或再加一句说明，用 %w）返回时，外层的 DispatchEvent 认出它，不再按外层的事件链再包一层。超出上限的错误
// （ErrEventLimit）同样已带完整事件链，也原样外传。
type EventError struct {
	Chain    string
	Listener string
	Err      error
}

func (e *EventError) Error() string {
	return e.Chain + " listener " + e.Listener + ": " + e.Err.Error()
}

func (e *EventError) Unwrap() error { return e.Err }

// DispatchEvent 按 (kind, ev 的动态类型) 取出登记的回调，依次以 owner 执行，第一个错误即停，
// 错误包装成 *EventError（"<事件链> listener <listener>: <err>"，调用方仍可 errors.Is）；监听返回的错误里已经有
// EventError 或 ErrEventLimit（来自内层发布）时原样返回，不再包。没有监听时返回 nil。
// 只由生成的分发闭包调用（经 EntityBase.Publish），锁检查与事件栈由 Publish 负责。
func DispatchEvent(kind EntityKind, owner IThreadSafeEntity, ev any) error {
	freezeEvents()
	if ev == nil {
		return errors.New("entity event: dispatch nil event")
	}
	typ := reflect.TypeOf(ev)
	listeners := eventListeners[eventKey{kind: kind, typ: typ}]
	for _, listener := range listeners {
		if err := listener.fn(owner, ev); err != nil {
			var inner *EventError
			if errors.As(err, &inner) || errors.Is(err, ErrEventLimit) {
				return err
			}
			return &EventError{Chain: ownerEventChain(owner, typ), Listener: listener.name, Err: err}
		}
	}
	return nil
}

// ownerEventChain 返回 owner 当前的事件链；Publish 之外直接调用 DispatchEvent 时栈为空，只写事件本身。
func ownerEventChain(owner IThreadSafeEntity, typ reflect.Type) string {
	if owner != nil {
		if base := owner.Base(); base != nil && len(base.events.types) > 0 {
			return eventChain(base.events.types, nil)
		}
	}
	return eventTypeName(typ)
}

// eventChain 把栈上的事件（以及尚未压栈的 next）拼成 "A → B → C"。
func eventChain(types []reflect.Type, next reflect.Type) string {
	var b strings.Builder
	for i, typ := range types {
		if i > 0 {
			b.WriteString(" → ")
		}
		b.WriteString(eventTypeName(typ))
	}
	if next != nil {
		if len(types) > 0 {
			b.WriteString(" → ")
		}
		b.WriteString(eventTypeName(next))
	}
	return b.String()
}

func countEvent(types []reflect.Type, typ reflect.Type) int {
	n := 0
	for _, t := range types {
		if t == typ {
			n++
		}
	}
	return n
}

// eventTypeName 取事件的类型名（game/event 下的结构体名就是事件名）；无名类型退回完整类型串。
func eventTypeName(typ reflect.Type) string {
	if typ == nil {
		return "<nil>"
	}
	if name := typ.Name(); name != "" {
		return name
	}
	return typ.String()
}
