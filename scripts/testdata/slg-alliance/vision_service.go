package alliance

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// VisionService 是教学 game 服务的视野生命周期。Init 只装配，Serve 独占刷新，
// Shutdown 先取消再等待；超时不谎报已排空，App 应保留其依赖 Mod，之后可重试等待。
// Refresh/Observe 在 Init 前设置，运行期不修改；回调必须遵守 ctx，不能另起无界后台工作。
// 实际 game 有其他业务时，把同一循环作为其 Serve/Shutdown 的一个受管 worker。
type VisionService struct {
	Interval, ReadTimeout time.Duration
	Refresh               func(context.Context, uint64) error
	Observe               func(error)
	mu                    sync.Mutex
	loop                  *permissionLoop
	ctx                   context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	running, stopping     bool
	doneOnce              sync.Once
}

var _ app.Service = (*VisionService)(nil)

func (*VisionService) Name() app.ServiceName { return "game" }
func (s *VisionService) Init(_ *app.Registry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loop != nil || s.Refresh == nil {
		return errors.New("vision service: invalid initialization")
	}
	loop, err := newPermissionLoop(s.Interval, s.ReadTimeout)
	if err != nil {
		return err
	}
	s.loop = loop
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.done = make(chan struct{})
	return nil
}
func (s *VisionService) Serve(ctx context.Context) error {
	s.mu.Lock()
	if s.loop == nil || s.running {
		s.mu.Unlock()
		return errors.New("vision service: invalid serve state")
	}
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.running = true
	s.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	if s.ctx.Err() != nil {
		cancel()
	}
	defer s.doneOnce.Do(func() { close(s.done) })
	err := s.loop.run(runCtx, s.Refresh, s.Observe)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func (s *VisionService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.loop == nil {
		s.mu.Unlock()
		return nil
	} // Init 失败后的清理。
	s.stopping = true
	s.cancel()
	if !s.running {
		s.doneOnce.Do(func() { close(s.done) })
	}
	done := s.done
	s.mu.Unlock()
	// 已排空时，即使调用者的期限刚到，也允许幂等关闭返回成功。
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
