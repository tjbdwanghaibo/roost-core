package nettransport

// AsyncTransportCounters 无需遍历会话。字段分别读取，不保证同一瞬间的事务快照；
// 驻留额度由原子预留维护，只有消息真正释放后才归还。
type AsyncTransportCounters struct {
	ActiveSessions                                            int64
	ResidentReliableBytes                                     int64
	ReliableQueued, ReliableSent, ReliableBytesSent           uint64
	ReliableBackpressure, GlobalBackpressure, ReliableExpired uint64
	ReliableAbandoned, SendErrors                             uint64
}

func (transport *AsyncTransport) Counters() AsyncTransportCounters {
	if transport == nil {
		return AsyncTransportCounters{}
	}
	s := &transport.stats
	return AsyncTransportCounters{
		ActiveSessions: s.activeSessions.Load(), ResidentReliableBytes: s.residentBytes.Load(),
		ReliableQueued: s.reliableQueued.Load(), ReliableSent: s.reliableSent.Load(), ReliableBytesSent: s.reliableBytesSent.Load(),
		ReliableBackpressure: s.reliableBackpressure.Load(), GlobalBackpressure: s.globalBackpressure.Load(), ReliableExpired: s.expired.Load(),
		ReliableAbandoned: s.reliableAbandoned.Load(), SendErrors: s.sendErrors.Load(),
	}
}

func (transport *AsyncTransport) reserveReliableBytes(bytes int64) bool {
	for {
		used := transport.stats.residentBytes.Load()
		if limit := transport.config.MaxResidentReliableBytes; limit > 0 && bytes > limit-used {
			return false
		}
		if transport.stats.residentBytes.CompareAndSwap(used, used+bytes) {
			return true
		}
	}
}
