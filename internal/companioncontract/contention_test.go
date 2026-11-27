package companioncontract_test

import (
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// 主、从绑定时共用主实体的物理锁。观察第二个 Nest 的真实物理竞争，不把
// GetMutex 的业务拒绝或同一引擎中尚未调度的请求误认为保锁证明。
type observedMutex struct {
	lock.Mutex
	busy chan struct{}
}

func (m *observedMutex) TryLock() bool {
	if m.Mutex.TryLock() {
		return true
	}
	select {
	case m.busy <- struct{}{}:
	default:
	}
	return false
}

func (f *fixture) observer(t *testing.T) *fixture {
	t.Helper()
	return &fixture{m: f.m, n: nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(f.m))), ctx: f.ctx, rootLock: f.rootLock}
}

type reply struct {
	value any
	err   error
}

// 仅在首个请求已停在测试闸门、且没有其他活跃请求时使用。返回时必须已看到
// 新请求尝试并失去底层锁；随后调用方放行首个请求，并校验同一竞争者的真实回复。
func (f *fixture) contend(h nest.HandlerName, id int64) (<-chan reply, error) {
	select {
	case <-f.rootLock.busy: // 上一轮重排可能留下通知。
	default:
	}
	done := make(chan reply, 1)
	go func() {
		v, err := f.n.Request(f.ctx, h, id, nil)
		done <- reply{v, err}
	}()
	select {
	case <-f.rootLock.busy:
		return done, nil
	case early := <-done:
		return nil, fmt.Errorf("request for %d finished before physical contention: %+v", id, early)
	case <-f.ctx.Done():
		return nil, fmt.Errorf("request for %d never contended: %w", id, f.ctx.Err())
	}
}
