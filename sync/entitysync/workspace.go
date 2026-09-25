package entitysync

// Flush 工作区由 flushGate 独占。保留容量有界，回收前清空全部业务引用；
// 不缓存最终帧字节，Transport 可在 Push 返回后继续持有它们。
func (m *Manager) takeFlushSession(sess *session) *flushSession {
	var batch *flushSession
	if n := len(m.flushPool); n > 0 {
		batch = m.flushPool[n-1]
		m.flushPool[n-1] = nil
		m.flushPool = m.flushPool[:n-1]
	} else {
		batch = &flushSession{}
	}
	batch.session = sess
	return batch
}

func (m *Manager) recycleFlush(work map[SessionID]*flushSession) {
	for _, batch := range work {
		clear(batch.entries)
		clear(batch.settlements)
		batch.entries, batch.settlements = batch.entries[:0], batch.settlements[:0]
		if cap(batch.entries) > 256 {
			batch.entries = nil
		}
		if cap(batch.settlements) > 256 {
			batch.settlements = nil
		}
		batch.session, batch.snapshotAfter = nil, 0
		if len(m.flushPool) < 2048 {
			m.flushPool = append(m.flushPool, batch)
		}
	}
	if len(work) <= 2048 {
		clear(work)
		m.flushWork = work
	} else {
		m.flushWork = nil
	}
}
