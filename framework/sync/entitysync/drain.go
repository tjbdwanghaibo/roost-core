package entitysync

import (
	"context"
	"time"
)

// Drain 在生产者停止后排空已登记的同步工作。预算/水位暂缓时等到下个周期，
// 传输失败或 context 取消明确返回错误；调用方应保留传输和水位来源直到完成。
// 它不承诺客户端已接收字节，传输队列仍需由其拥有者排空。
func (m *Manager) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := m.Flush(ctx); err != nil {
			return err
		}
		m.pendingMu.Lock()
		pending := len(m.pending) + len(m.waitingSnapshots)
		m.pendingMu.Unlock()
		if pending == 0 && !m.policiesPending() {
			return nil
		}
		timer := time.NewTimer(m.config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
