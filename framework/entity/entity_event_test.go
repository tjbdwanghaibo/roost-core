package entity

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// 实体事件的单测。登记表在第一次 Publish 时冻结，所以本文件的全部监听都在 init 里登记，每个场景用自己的 kind。

const (
	eventKindOrder   EntityKind = 231 // 登记顺序、首错即停
	eventKindDepth   EntityKind = 232 // 嵌套深度上限
	eventKindReenter EntityKind = 233 // 同一事件重入上限
	eventKindPerRoot EntityKind = 234 // 一次最外层发布的总次数上限，以及无监听计数
	eventKindPanic   EntityKind = 235 // 监听 panic 后栈被清空
	eventKindNested  EntityKind = 236 // 内层监听出错时，错误原样外传不再逐层包装
)

type evOrder struct{ N int }
type evUnheard struct{}

type evD0 struct{}
type evD1 struct{}
type evD2 struct{}
type evD3 struct{}
type evD4 struct{}
type evD5 struct{}
type evD6 struct{}
type evD7 struct{}
type evD8 struct{}

type evLevelUp struct{ Level int }
type evAddExp struct{ Level int }

type evBurst struct{ Times int }
type evTick struct{}

type evBoom struct{}

type evNestOuter struct{}
type evNestMiddle struct{}
type evNestInner struct{}

var errEventBoom = errors.New("boom")

// eventTestEntity 是带事件分发的最小实体，log 记录监听的执行顺序。
type eventTestEntity struct {
	*EntityBase
	log []string
	// levelTarget 是重入场景里升到的目标等级；panics 控制 panic 场景的监听是否 panic。
	levelTarget int
	panics      bool
}

func (e *eventTestEntity) Base() *EntityBase { return e.EntityBase }

func newEventTestEntity(id int64, kind EntityKind) *eventTestEntity {
	e := &eventTestEntity{EntityBase: NewEntityBase(id, testEntityCategoryPlayer, false, kind)}
	e.SetEventDispatch(func(ev any) error { return DispatchEvent(kind, e, ev) })
	return e
}

func init() {
	// 顺序：first、second 记录；second 在 N=1 时返回错误，third 不应再执行。
	RegisterEventListener(eventKindOrder, "evOrder", "order.first", func(e *eventTestEntity, ev evOrder) error {
		e.log = append(e.log, "first")
		return nil
	})
	RegisterEventListener(eventKindOrder, "evOrder", "order.second", func(e *eventTestEntity, ev evOrder) error {
		e.log = append(e.log, "second")
		if ev.N == 1 {
			return errEventBoom
		}
		return nil
	})
	RegisterEventListener(eventKindOrder, "evOrder", "order.third", func(e *eventTestEntity, ev evOrder) error {
		e.log = append(e.log, "third")
		return nil
	})

	// 深度：D0 → D1 → … → D8，每层监听发布下一层；第 9 层超出缺省深度 8。
	registerDepthStep[evD0](evD1{}, "D0")
	registerDepthStep[evD1](evD2{}, "D1")
	registerDepthStep[evD2](evD3{}, "D2")
	registerDepthStep[evD3](evD4{}, "D3")
	registerDepthStep[evD4](evD5{}, "D4")
	registerDepthStep[evD5](evD6{}, "D5")
	registerDepthStep[evD6](evD7{}, "D6")
	registerDepthStep[evD7](evD8{}, "D7")
	registerDepthStep[evD8](nil, "D8")

	// 重入：LevelUp → AddExp → LevelUp …，直到等级到 levelTarget。
	RegisterEventListener(eventKindReenter, "evLevelUp", "reenter.reward", func(e *eventTestEntity, ev evLevelUp) error {
		e.log = append(e.log, fmt.Sprintf("level%d", ev.Level))
		if ev.Level >= e.levelTarget {
			return nil
		}
		return e.Publish(evAddExp{Level: ev.Level})
	})
	RegisterEventListener(eventKindReenter, "evAddExp", "reenter.exp", func(e *eventTestEntity, ev evAddExp) error {
		return e.Publish(evLevelUp{Level: ev.Level + 1})
	})

	// 总次数：Burst 的监听顺序发布 Times 次没有监听的 Tick。
	RegisterEventListener(eventKindPerRoot, "evBurst", "perroot.burst", func(e *eventTestEntity, ev evBurst) error {
		for i := 0; i < ev.Times; i++ {
			if err := e.Publish(evTick{}); err != nil {
				return err
			}
			e.log = append(e.log, "tick")
		}
		return nil
	})

	// panic：Boom 的监听先再发布一层，再按开关 panic。
	RegisterEventListener(eventKindPanic, "evBoom", "panic.outer", func(e *eventTestEntity, ev evBoom) error {
		return e.Publish(evOrder{})
	})
	RegisterEventListener(eventKindPanic, "evOrder", "panic.inner", func(e *eventTestEntity, ev evOrder) error {
		if e.panics {
			panic("listener panic")
		}
		e.log = append(e.log, "inner")
		return nil
	})

	// 嵌套出错：Outer → Middle → Inner，Inner 的监听返回 boom；外两层原样返回内层错误。
	RegisterEventListener(eventKindNested, "evNestOuter", "nested.outer", func(e *eventTestEntity, ev evNestOuter) error {
		return e.Publish(evNestMiddle{})
	})
	RegisterEventListener(eventKindNested, "evNestMiddle", "nested.middle", func(e *eventTestEntity, ev evNestMiddle) error {
		if err := e.Publish(evNestInner{}); err != nil {
			// 监听在内层错误外再加一句说明也照样认得出：不再按事件链包一层。
			return fmt.Errorf("middle: %w", err)
		}
		return nil
	})
	RegisterEventListener(eventKindNested, "evNestInner", "nested.inner", func(e *eventTestEntity, ev evNestInner) error {
		return errEventBoom
	})
}

func registerDepthStep[E any](next any, name string) {
	RegisterEventListener(eventKindDepth, name, "depth."+name, func(e *eventTestEntity, ev E) error {
		e.log = append(e.log, name)
		if next == nil {
			return nil
		}
		return e.Publish(next)
	})
}

// withLockedEntity 在新的 Guard 作用域里锁住 ent 后执行 fn。
func withLockedEntity(t *testing.T, ent IThreadSafeEntity, fn func() error) error {
	t.Helper()
	return WithGuardScope(entitypermit.System(), "entity_event_test", func(scope *GuardScope) error {
		if !scope.Guard().RequireEntity(entitypermit.System(), ent) {
			t.Fatal("RequireEntity failed")
		}
		return fn()
	})
}

func TestEntityEventPublishRequiresGuard(t *testing.T) {
	e := newEventTestEntity(9_100_001, eventKindOrder)
	if err := e.Publish(evOrder{}); !errors.Is(err, ErrEventNotGuarded) {
		t.Fatalf("publish without any guard: got %v, want ErrEventNotGuarded", err)
	}
	if len(e.log) != 0 {
		t.Fatalf("listeners ran without the lock: %v", e.log)
	}

	// Guard 持有的是另一个实体。
	other := newEventTestEntity(9_100_002, eventKindOrder)
	err := withLockedEntity(t, other, func() error { return e.Publish(evOrder{}) })
	if !errors.Is(err, ErrEventNotGuarded) {
		t.Fatalf("publish while the guard holds another entity: got %v, want ErrEventNotGuarded", err)
	}

	// 同 ID、不同锁的另一个实例：Guard 持有的不是它的锁。
	twin := newEventTestEntity(9_100_001, eventKindOrder)
	err = withLockedEntity(t, e, func() error { return twin.Publish(evOrder{}) })
	if !errors.Is(err, ErrEventNotGuarded) {
		t.Fatalf("publish on a same-id instance with another mutex: got %v, want ErrEventNotGuarded", err)
	}

	// 锁住之后可以发布。
	if err := withLockedEntity(t, e, func() error { return e.Publish(evOrder{}) }); err != nil {
		t.Fatalf("publish under the guard: %v", err)
	}
	if got := strings.Join(e.log, ","); got != "first,second,third" {
		t.Fatalf("listeners = %q", got)
	}
}

func TestEntityEventPublishWithoutDispatch(t *testing.T) {
	e := &eventTestEntity{EntityBase: NewEntityBase(9_100_010, testEntityCategoryPlayer, false, eventKindOrder)}
	err := withLockedEntity(t, e, func() error { return e.Publish(evOrder{}) })
	if !errors.Is(err, ErrEventDispatchMissing) {
		t.Fatalf("publish without SetEventDispatch: got %v, want ErrEventDispatchMissing", err)
	}
}

func TestEntityEventListenersRunInOrderAndStopAtFirstError(t *testing.T) {
	e := newEventTestEntity(9_100_020, eventKindOrder)
	err := withLockedEntity(t, e, func() error { return e.Publish(evOrder{N: 1}) })
	if !errors.Is(err, errEventBoom) {
		t.Fatalf("got %v, want wrapped errEventBoom", err)
	}
	if want := "evOrder listener order.second: boom"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if got := strings.Join(e.log, ","); got != "first,second" {
		t.Fatalf("listeners = %q, want first,second (third must not run after the error)", got)
	}
	if len(e.events.types) != 0 || e.events.rootCount != 0 {
		t.Fatalf("event stack not empty after an error: %+v", e.events)
	}
}

func TestEntityEventNoListenerReturnsNil(t *testing.T) {
	e := newEventTestEntity(9_100_030, eventKindOrder)
	if err := withLockedEntity(t, e, func() error { return e.Publish(evUnheard{}) }); err != nil {
		t.Fatalf("publish without listeners: %v", err)
	}
}

func TestEntityEventDepthLimit(t *testing.T) {
	e := newEventTestEntity(9_100_040, eventKindDepth)
	err := withLockedEntity(t, e, func() error { return e.Publish(evD0{}) })
	if !errors.Is(err, ErrEventLimit) {
		t.Fatalf("got %v, want ErrEventLimit", err)
	}
	chain := "evD0 → evD1 → evD2 → evD3 → evD4 → evD5 → evD6 → evD7 → evD8: nesting depth exceeds 8"
	if !strings.Contains(err.Error(), chain) {
		t.Fatalf("error %q does not carry the full chain %q", err, chain)
	}
	// D8 超限，它的监听不执行；前 8 层执行过。
	if got := strings.Join(e.log, ","); got != "D0,D1,D2,D3,D4,D5,D6,D7" {
		t.Fatalf("listeners = %q", got)
	}
	if len(e.events.types) != 0 {
		t.Fatalf("event stack not empty: %d", len(e.events.types))
	}
}

func TestEntityEventSameEventReentry(t *testing.T) {
	// LevelUp 在栈上同时出现 3 次（合法重入 2 次）可以通过。
	e := newEventTestEntity(9_100_050, eventKindReenter)
	e.levelTarget = 3
	if err := withLockedEntity(t, e, func() error { return e.Publish(evLevelUp{Level: 1}) }); err != nil {
		t.Fatalf("two legal re-entries: %v", err)
	}
	if got := strings.Join(e.log, ","); got != "level1,level2,level3" {
		t.Fatalf("listeners = %q", got)
	}

	// 第 3 次重入（栈上第 4 个 LevelUp）报错，早于深度上限。
	e = newEventTestEntity(9_100_051, eventKindReenter)
	e.levelTarget = 4
	err := withLockedEntity(t, e, func() error { return e.Publish(evLevelUp{Level: 1}) })
	if !errors.Is(err, ErrEventLimit) {
		t.Fatalf("third re-entry: got %v, want ErrEventLimit", err)
	}
	chain := "evLevelUp → evAddExp → evLevelUp → evAddExp → evLevelUp → evAddExp → evLevelUp: evLevelUp is on the stack more than 3 times"
	if !strings.Contains(err.Error(), chain) {
		t.Fatalf("error %q does not carry %q", err, chain)
	}
	if got := strings.Join(e.log, ","); got != "level1,level2,level3" {
		t.Fatalf("listeners = %q", got)
	}
}

func TestEntityEventPerRootLimitCountsUnheardEvents(t *testing.T) {
	// 1 次 Burst + 255 次 Tick = 256 次，正好到上限。Tick 没有监听，也计数。
	e := newEventTestEntity(9_100_060, eventKindPerRoot)
	if err := withLockedEntity(t, e, func() error { return e.Publish(evBurst{Times: 255}) }); err != nil {
		t.Fatalf("256 publishes within one root: %v", err)
	}

	// 计数按最外层发布清零：同一实体上的下一次最外层发布重新计数。
	e.log = nil
	err := withLockedEntity(t, e, func() error { return e.Publish(evBurst{Times: 300}) })
	if !errors.Is(err, ErrEventLimit) {
		t.Fatalf("257th publish: got %v, want ErrEventLimit", err)
	}
	if !strings.Contains(err.Error(), "evBurst → evTick: more than 256 publishes within one outermost Publish") {
		t.Fatalf("error %q does not carry the chain", err)
	}
	if len(e.log) != 255 {
		t.Fatalf("ticks before the limit = %d, want 255", len(e.log))
	}
}

// 嵌套发布出错时，最内层的事件错误已经带着完整事件链和出错的监听；外层原样返回，不再逐层包装。
func TestEntityEventNestedErrorIsNotRewrapped(t *testing.T) {
	e := newEventTestEntity(9_100_090, eventKindNested)
	err := withLockedEntity(t, e, func() error { return e.Publish(evNestOuter{}) })
	if !errors.Is(err, errEventBoom) {
		t.Fatalf("got %v, want wrapped errEventBoom", err)
	}
	if want := "middle: evNestOuter → evNestMiddle → evNestInner listener nested.inner: boom"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	var eventErr *EventError
	if !errors.As(err, &eventErr) || eventErr.Listener != "nested.inner" || eventErr.Chain != "evNestOuter → evNestMiddle → evNestInner" {
		t.Fatalf("errors.As EventError = %+v", eventErr)
	}

	// 超出上限的错误同样只出现一次，外层不包。
	d := newEventTestEntity(9_100_091, eventKindDepth)
	err = withLockedEntity(t, d, func() error { return d.Publish(evD0{}) })
	want := "entity event: limit exceeded: evD0 → evD1 → evD2 → evD3 → evD4 → evD5 → evD6 → evD7 → evD8: nesting depth exceeds 8"
	if err == nil || err.Error() != want {
		t.Fatalf("depth error = %q, want %q", err, want)
	}
}

func TestEntityEventPanicClearsStack(t *testing.T) {
	e := newEventTestEntity(9_100_070, eventKindPanic)
	e.panics = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("listener panic was swallowed")
			}
		}()
		_ = withLockedEntity(t, e, func() error { return e.Publish(evBoom{}) })
	}()
	if len(e.events.types) != 0 || e.events.rootCount != 0 {
		t.Fatalf("event stack after panic = %+v, want empty", e.events)
	}

	// 下一笔不受影响。
	e.panics = false
	if err := withLockedEntity(t, e, func() error { return e.Publish(evBoom{}) }); err != nil {
		t.Fatalf("publish after panic: %v", err)
	}
	if got := strings.Join(e.log, ","); got != "inner" {
		t.Fatalf("listeners = %q", got)
	}
}

func TestEntityEventRegisterAfterFirstPublishPanics(t *testing.T) {
	e := newEventTestEntity(9_100_080, eventKindOrder)
	_ = e.Publish(evOrder{}) // 未持锁也算首次发布：冻结点在入口。
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "after the first Publish") {
			t.Fatalf("register after the first Publish: recover = %v", r)
		}
	}()
	RegisterEventListener(eventKindOrder, "evOrder", "order.late", func(*eventTestEntity, evOrder) error { return nil })
}

func TestEntityEventDuplicateListenerPanics(t *testing.T) {
	// 冻结与否和本例无关：临时解冻，只验证重复登记本身。
	frozen := eventFrozen.Load()
	eventFrozen.Store(false)
	defer eventFrozen.Store(frozen)
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "duplicate listener order.first") {
			t.Fatalf("duplicate registration: recover = %v", r)
		}
	}()
	RegisterEventListener(eventKindOrder, "evOrder", "order.first", func(*eventTestEntity, evOrder) error { return nil })
}
