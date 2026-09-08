package remoteentity

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mirror"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

// Stats is the capacity picture the Mod's health check publishes.
type Stats struct {
	Wrappers           int
	LocalInterests     int
	Transactions       int
	ActiveTransactions int
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
	m.remote.localInterestMu.Lock()
	stats.LocalInterests = len(m.remote.localInterests)
	m.remote.localInterestMu.Unlock()
	m.remote.txMu.Lock()
	stats.Transactions = len(m.remote.txs)
	for _, tracker := range m.remote.txs {
		if !tracker.closed {
			stats.ActiveTransactions++
		}
	}
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
