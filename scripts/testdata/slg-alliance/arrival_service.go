package alliance

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// ArrivalWorker 每轮只发现有界持久待办，失败不记进度；下轮重新发现，绝不换事件身份。
// 本轮预算包括所有 Consume，不为每个事件另起 goroutine 或重新延长超时。
type ArrivalWorker struct {
	Limit    int
	Discover func(context.Context, int) ([]string, error)
	Consume  func(context.Context, string) error
}

func (w ArrivalWorker) validate() error {
	if w.Limit < 1 || w.Limit > 128 || w.Discover == nil || w.Consume == nil {
		return errors.New("arrival worker: invalid configuration")
	}
	return nil
}
func (w ArrivalWorker) Tick(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keys, err := w.Discover(ctx, w.Limit)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if len(keys) > w.Limit {
		return errors.New("arrival worker: discovery exceeded limit")
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if key == "" || len(key) > 64 || seen[key] {
			return errors.New("arrival worker: invalid discovered identity")
		}
		seen[key] = true
	}
	for _, key := range keys {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = w.Consume(ctx, key); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// ArrivalService 复用已验证的 App Service 启停：常驻 ticker、每轮期限、取消与可重试排空。
// worker 与视野发布分开，读视图不产生报告；实际 game 可把它作为独立受管 worker。
type ArrivalService struct {
	VisionService
	Worker ArrivalWorker
}

var _ app.Service = (*ArrivalService)(nil)

func (s *ArrivalService) Init(r *app.Registry) error {
	if err := s.Worker.validate(); err != nil {
		return err
	}
	s.Refresh = func(ctx context.Context, _ uint64) error { return s.Worker.Tick(ctx) }
	return s.VisionService.Init(r)
}
