package cellworld

import (
	"sync"
	"time"
)

// loopback 是进程内的 Transport 替身：每个订阅一个投递 goroutine 和有界队列（满了丢弃，与 NATS 慢消费者一致），
// fault 可以对每条消息决定丢弃、重复或延迟（延迟用 AfterFunc，所以会乱序）。
type loopback struct {
	mu    sync.Mutex
	subs  map[string][]*loopSub
	fault func(subject string, m *wireMsg) (dropIt, dupIt bool, delay time.Duration)
}

type loopSub struct {
	ch      chan []byte
	done    chan struct{}
	stopped chan struct{}
}

func newLoopback() *loopback { return &loopback{subs: map[string][]*loopSub{}} }

func (l *loopback) Subscribe(subject string, handler func([]byte)) (func() error, error) {
	s := &loopSub{ch: make(chan []byte, 65536), done: make(chan struct{}), stopped: make(chan struct{})}
	l.mu.Lock()
	l.subs[subject] = append(l.subs[subject], s)
	l.mu.Unlock()
	go func() {
		defer close(s.stopped)
		for {
			select {
			case <-s.done:
				return
			case data := <-s.ch:
				handler(data)
			}
		}
	}()
	var once sync.Once
	return func() error {
		once.Do(func() {
			l.mu.Lock()
			list := l.subs[subject]
			for i, x := range list {
				if x == s {
					l.subs[subject] = append(list[:i:i], list[i+1:]...)
					break
				}
			}
			l.mu.Unlock()
			close(s.done)
			<-s.stopped
		})
		return nil
	}, nil
}

func (l *loopback) Publish(subject string, data []byte) error {
	l.mu.Lock()
	subs := append([]*loopSub(nil), l.subs[subject]...)
	fault := l.fault
	l.mu.Unlock()
	copies := 1
	var delay time.Duration
	if fault != nil {
		m, err := decodeMsg(data)
		if err == nil {
			dropIt, dupIt, d := fault(subject, m)
			if dropIt {
				return nil
			}
			if dupIt {
				copies = 2
			}
			delay = d
		}
	}
	for range copies {
		cp := append([]byte(nil), data...)
		for _, s := range subs {
			enqueue := func() {
				select {
				case <-s.done:
				case s.ch <- cp:
				default: // 队列满：丢弃
				}
			}
			if delay > 0 {
				time.AfterFunc(delay, enqueue)
			} else {
				enqueue()
			}
		}
	}
	return nil
}

func (l *loopback) setFault(f func(subject string, m *wireMsg) (bool, bool, time.Duration)) {
	l.mu.Lock()
	l.fault = f
	l.mu.Unlock()
}
