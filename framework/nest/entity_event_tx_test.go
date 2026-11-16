// 实体事件与 Nest 事务：监听在 handler 的同一笔事务里执行，监听（含嵌套发布里的监听）出错时整笔回滚，
// handler 与先执行的监听所做的修改一起撤销；全部成功时一起提交。契约见 docs/framework/guide/02-nest-entity.md §3A。

package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

type txEventOuter struct{ Add int }

type txEventInner struct{}

var (
	errTxEventInner = errors.New("inner refused")
	// txEventInnerFails 由用例设置；用例不并行。
	txEventInnerFails bool
	txEventOrder      []string
)

func init() {
	entity.RegisterEventListener(nestLocalKind, "txEventOuter", "nest.test.outerAdd", func(e *rollbackTestEntity, ev txEventOuter) error {
		txEventOrder = append(txEventOrder, "outerAdd")
		e.dao.Value += ev.Add
		return nil
	})
	entity.RegisterEventListener(nestLocalKind, "txEventOuter", "nest.test.outerNested", func(e *rollbackTestEntity, _ txEventOuter) error {
		txEventOrder = append(txEventOrder, "outerNested")
		// 内层错误必须原样外传。
		return e.Publish(txEventInner{})
	})
	entity.RegisterEventListener(nestLocalKind, "txEventInner", "nest.test.innerAdd", func(e *rollbackTestEntity, _ txEventInner) error {
		txEventOrder = append(txEventOrder, "innerAdd")
		e.dao.Value += 1000
		if txEventInnerFails {
			return errTxEventInner
		}
		return nil
	})
}

func TestEntityEventListenerFailureRollsBackTheWholeTransaction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		innerFails bool
		durability DurabilityPolicy
		want       int
	}{
		{name: "listeners_succeed_memory", durability: DurabilityMemory, want: 1111},
		{name: "listeners_succeed_strict", durability: DurabilityStrict, want: 1111},
		{name: "nested_listener_fails_memory", innerFails: true, durability: DurabilityMemory, want: 10},
		{name: "nested_listener_fails_strict", innerFails: true, durability: DurabilityStrict, want: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, target := newAsyncPilotEntity(t, 9960, 10)
			target.SetEventDispatch(func(ev any) error { return entity.DispatchEvent(nestLocalKind, target, ev) })
			getter := newMockGetter()
			getter.Add(target)
			committer := &recordingCommitter{}
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			handler := NewHandlerName("entity_event_tx_" + tc.name)
			engine.MustRegisterHandlerWithMeta(handler, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				e := es[0].(*rollbackTestEntity)
				e.dao.Value = 11
				return nil, e.Publish(txEventOuter{Add: 100})
			}, HandlerMeta{Rollback: RollbackState, Durability: tc.durability})
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())

			txEventInnerFails, txEventOrder = tc.innerFails, nil
			t.Cleanup(func() { txEventInnerFails, txEventOrder = false, nil })
			_, err := engine.Request(context.Background(), handler, id, nil)

			if got := strings.Join(txEventOrder, ","); got != "outerAdd,outerNested,innerAdd" {
				t.Errorf("listener order = %s, want registration order outerAdd,outerNested,innerAdd", got)
			}
			if target.dao.Value != tc.want {
				t.Errorf("value after the request = %d, want %d", target.dao.Value, tc.want)
			}
			if !tc.innerFails {
				if err != nil {
					t.Fatalf("request error = %v", err)
				}
				return
			}
			if !errors.Is(err, errTxEventInner) {
				t.Fatalf("request error = %v, want the inner listener's error", err)
			}
			var eventErr *entity.EventError
			if !errors.As(err, &eventErr) {
				t.Fatalf("request error %v is not an *entity.EventError", err)
			}
			if eventErr.Listener != "nest.test.innerAdd" || !strings.Contains(eventErr.Chain, "txEventOuter") || !strings.Contains(eventErr.Chain, "txEventInner") {
				t.Errorf("event error = listener %q chain %q, want the inner listener with the full chain", eventErr.Listener, eventErr.Chain)
			}
			if strings.Count(err.Error(), "listener ") != 1 {
				t.Errorf("event error wrapped more than once: %v", err)
			}
			if tc.durability == DurabilityStrict && committer.record.Handler != "" {
				t.Errorf("a rolled-back transaction reached the committer: %+v", committer.record)
			}
		})
	}
}
