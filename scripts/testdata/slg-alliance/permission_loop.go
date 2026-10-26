package alliance

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// permissionLoop 串行协调一个视野分区。hint 只加速读取，定期重读覆盖消息丢失。
// refresh 必须先 hold，且 load 必须遵守 context；不允许另一个 goroutine 用旧缓存 Flush/Ready。
// 最多保存一个唤醒和最高代际，通知风暴不能形成无界队列，也不能把高代际覆盖成低代际。
type permissionLoop struct {
	interval, timeout time.Duration
	minimum           atomic.Uint64
	wake              chan struct{}
}

func newPermissionLoop(interval, timeout time.Duration) (*permissionLoop, error) {
	if interval <= 0 || timeout <= 0 {
		return nil, errors.New("positive permission loop intervals required")
	}
	return &permissionLoop{interval: interval, timeout: timeout, wake: make(chan struct{}, 1)}, nil
}
func (p *permissionLoop) notify(generation uint64) {
	for old := p.minimum.Load(); generation > old; old = p.minimum.Load() {
		if p.minimum.CompareAndSwap(old, generation) {
			break
		}
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// observe 是运维／验收回调，不能阻塞；单次读取失败保持 held 并等下一轮，不重建旧权限。
func (p *permissionLoop) run(ctx context.Context, refresh func(context.Context, uint64) error, observe func(error)) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		readCtx, cancel := context.WithTimeout(ctx, p.timeout)
		e := refresh(readCtx, p.minimum.Load())
		cancel()
		if observe != nil {
			observe(e)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-p.wake:
		}
	}
}
