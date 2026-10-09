package syncbus

import (
	"crypto/rand"
	"strconv"
	"sync/atomic"
)

// DeliveryIDs mints the delivery identity a SyncMsg carries in MessageID.
//
// The identity is independent of the business Version on purpose. A JetStream
// transport uses MessageID as its deduplication key, and when a message has
// none it falls back to the tuple (topic, key, version, sid, part). That tuple
// is not an identity: an upsert and a delete at the same version share it, a
// publisher that sets no sid produces no key at all, and a publisher that
// restarts and reissues a sequence shares it with its former self inside the
// broker's dedup window — so the second message is dropped, silently.
//
// Each instance carries a random, process-unique prefix and a monotonic
// counter. Two publishers, or one publisher before and after a restart, never
// mint the same id, and a single publisher never repeats one. Every SyncMsg
// publisher in core and kit — PatchSyncer, the mirror Replicator, the
// syncstream Publisher — mints through this type, so there is one identity
// rule rather than one per publish path.
type DeliveryIDs struct {
	prefix   string
	sequence atomic.Uint64
}

// NewDeliveryIDs returns a minter whose ids read "<kind>:<random>:<n>". kind
// names the publish path for a human reading a broker's dedup log; it is not
// part of the uniqueness, the random segment is.
func NewDeliveryIDs(kind string) *DeliveryIDs {
	if kind == "" {
		kind = "sync"
	}
	return &DeliveryIDs{prefix: kind + ":" + rand.Text() + ":"}
}

// Next mints the next identity. It is safe for concurrent use.
func (d *DeliveryIDs) Next() string {
	return d.prefix + strconv.FormatUint(d.sequence.Add(1), 10)
}
