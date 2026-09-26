// Package operation 管理可取消关闭等待的在途调用所有权。
package operation

import "sync"

// Lifetime 零值可用。Stop 阻止新调用；已准入调用仍需 End，不能由取消代为归还。
type Lifetime struct {
	mu       sync.Mutex
	active   int
	stopping bool
	drained  chan struct{}
}

func (l *Lifetime) Begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.active++
	return true
}
func (l *Lifetime) End() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.stopping && l.active == 0 {
		close(l.drained)
	}
}
func (l *Lifetime) Stop() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopping {
		l.stopping = true
		l.drained = make(chan struct{})
		if l.active == 0 {
			close(l.drained)
		}
	}
	return l.drained
}
