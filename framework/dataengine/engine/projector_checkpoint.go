package engine

import (
	"context"
	"sync"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
)

// replayCheckpointWriter 只活在一次 ReplayPass 内。请求保存最新成功前缀的下标，
// 不保存第二份记录；返回 ReplayPass 前必须 close，不能把异步工作留给下轮或 Shutdown。
type replayCheckpointWriter struct {
	mu                sync.Mutex
	target, confirmed int
	err               error
	closing           bool
	changed           chan struct{}
	wake              chan struct{}
	done              chan struct{}
}

func newReplayCheckpointWriter(ctx context.Context, p *Projector, records []corenest.CommitRecord, fences []corenest.CommitFence) *replayCheckpointWriter {
	w := &replayCheckpointWriter{changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			w.mu.Lock()
			target, from, closing := w.target, w.confirmed, w.closing
			w.mu.Unlock()
			if target == from {
				if closing {
					return
				}
				<-w.wake
				continue
			}
			start := hoptrace.Now()
			err := p.ack(ctx, fences[target-1])
			if err == nil {
				p.acknowledge(records[from:target])
				hoptrace.Since("h09.de.checkpoint", start)
			} else {
				p.observeAckError(err)
			}
			w.mu.Lock()
			if err == nil {
				w.confirmed = target
			} else {
				w.err = err
			}
			w.notifyLocked()
			w.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return w
}

func (w *replayCheckpointWriter) notifyLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *replayCheckpointWriter) submit(target int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.target = max(w.target, target)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

func (w *replayCheckpointWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// 等的目标在调用处固定；只有当前 ReplayPass 提交请求，等待期间不会有人扩大目标。
// 不因 ctx 到期提前离开：WAL.Ack 的物理写入可能仍在进行，生命周期必须等它结束。
func (w *replayCheckpointWriter) wait() error {
	for {
		w.mu.Lock()
		err, complete, changed := w.err, w.confirmed >= w.target, w.changed
		w.mu.Unlock()
		if err != nil || complete {
			return err
		}
		<-changed
	}
}

func (w *replayCheckpointWriter) close() {
	w.mu.Lock()
	w.closing = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	<-w.done
}
