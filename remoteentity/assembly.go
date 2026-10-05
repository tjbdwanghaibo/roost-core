package remoteentity

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// Stats is the capacity picture the Mod's health check publishes.
type Stats struct {
	Wrappers           int
	LocalInterests     int
	Transactions       int
	ActiveTransactions int
	WritesInFlight     int
	WriteLimit         int
	WriteRejected      uint64
}

// Stats snapshots wrapper, interest and transaction counts. Assembly code
// compares them against configured limits; the manager itself enforces
// those limits at admission time.
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
	m.remote.localInterestMu.Lock()
	// RR-20261005-NC-131：过期条目只在新建兴趣且表满、或每 1024 次续租时清理，空闲进程里会一直留着。
	// 健康检查拿这个数和上限比，所以表满时先清掉过期的再数——否则一阵读取把表读满之后，即使全部
	// 兴趣早已过期，健康也一直报 capacity exhausted。只在表满时清理，平时不做全表扫描。
	if capacity := m.remote.localInterestCapacity; capacity > 0 && len(m.remote.localInterests) >= capacity {
		m.pruneLocalInterestsLocked(time.Now().UnixNano())
	}
	stats.LocalInterests = len(m.remote.localInterests)
	m.remote.localInterestMu.Unlock()
	m.remote.txMu.Lock()
	stats.Transactions = len(m.remote.txs)
	stats.ActiveTransactions = stats.Transactions - m.remote.closedCount
	m.remote.txMu.Unlock()
	return stats
}

// Backend returns the authoritative backend the manager was sealed with, so
// the Mod can run storage initialisation before recovery.
func (m *Manager) Backend() entity.IRemoteEntityBackend {
	if m == nil {
		return nil
	}
	return m.backend
}

// BindSync wires snapshot and interest replication onto the given sync bus
// and installs the resulting syncer on the manager. The replicators are
// returned unstarted; the caller starts them and owns their shutdown.
func (m *Manager) BindSync(bus fsyncbus.ISyncBus) (snapshotRep, interestRep *mirror.Replicator) {
	snapshotRep = mirror.New(bus, SyncTopicSnapshot, SnapshotReplicaStore{mgr: m})
	interestRep = mirror.New(bus, SyncTopicInterest, InterestReplicaStore{mgr: m})
	syncer := NewSyncer(snapshotRep)
	syncer.mgr = m
	syncer.interestRep = interestRep
	m.SetSyncer(syncer)
	return snapshotRep, interestRep
}
