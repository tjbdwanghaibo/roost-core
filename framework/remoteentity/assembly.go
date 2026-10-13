package remoteentity

import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
)

// Stats 是 Mod 健康检查发布的容量概况。
type Stats struct {
	Wrappers           int
	LocalInterests     int
	Transactions       int
	ActiveTransactions int
	WritesInFlight     int
	WriteLimit         int
	WriteRejected      uint64
	// SnapshotPush 报告快照推送是否开着（false 时读取按需回源）。
	SnapshotPush bool
	// InterestRefused 是本机兴趣续租被拒的累计次数（配额 / 表满）。
	InterestRefused uint64
}

// Stats 取 wrapper、兴趣与事务计数的快照。装配代码拿它与配置上限比较；上限本身由 Manager 在准入时强制。
func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	stats := Stats{Wrappers: m.WrapperCount()}
	if m.remote == nil {
		return stats
	}
	stats.WritesInFlight = len(m.remote.writeSlots)
	stats.WriteLimit = cap(m.remote.writeSlots)
	stats.WriteRejected = m.remote.writeRejected.Load()
	snapshots := m.snapshots.Stats()
	stats.LocalInterests, stats.SnapshotPush, stats.InterestRefused = snapshots.LocalInterests, snapshots.PushEnabled, snapshots.InterestRejected
	m.remote.txMu.Lock()
	stats.Transactions = len(m.remote.txs)
	stats.ActiveTransactions = stats.Transactions - m.remote.closedCount
	m.remote.txMu.Unlock()
	return stats
}

// Backend 返回 Manager 封存的权威后端，让 Mod 能在恢复之前初始化存储。
func (m *Manager) Backend() entity.IRemoteEntityBackend {
	if m == nil {
		return nil
	}
	return m.backend
}

// BindSync 把快照与兴趣复制接到给定的同步总线上，并把得到的 syncer 装到 Manager 上。返回的复制器尚未启动，
// 由调用方启动并负责停止。
// 正式装配用 Assembly.Start（经 SnapshotClient.Start 管订阅与停机）；这里保留给自管复制器的旧调用方。
func (m *Manager) BindSync(bus fsyncbus.ISyncBus) (snapshotRep, interestRep *mirror.Replicator) {
	m.snapshots.mu.Lock()
	defer m.snapshots.mu.Unlock()
	return m.snapshots.bindLocked(bus, false)
}
