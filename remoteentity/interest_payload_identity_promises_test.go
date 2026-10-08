package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// RR-20261005-NC-34：兴趣订阅的 payload 必须绑定信封 key/版本/操作。
// 旧实现可用一个消费者的信封续租或撤销另一个 scope/SID 的 lease。
func TestInterestPayloadIdentityBeforeRegistryMutation(t *testing.T) {
	key := interestKeyFor(t, 244, 9350)
	base := entity.RemoteSnapshotInterest{Key: key, ConsumerSID: 7, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 11}
	for _, release := range []bool{false, true} {
		for _, field := range []string{"scope", "sid", "expires", "operation"} {
			name := "renew/" + field
			if release {
				name = "release/" + field
			}
			t.Run(name, func(t *testing.T) {
				m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
				bus := &interestIdentityBus{}
				_, receiver := m.BindSync(bus)
				if err := receiver.Start(); err != nil {
					t.Fatal(err)
				}
				defer receiver.Stop()
				payload := base
				op := mirror.OpUpsert
				switch field {
				case "scope":
					payload.Key.Scope++
				case "sid":
					payload.ConsumerSID++
				case "expires":
					payload.ExpiresAt++
				case "operation":
					op = mirror.OpDelete
				}
				seed := payload
				seed.Generation = 10
				if err := m.snapshots.interests.renew(seed); err != nil {
					t.Fatal(err)
				}
				raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: payload, Release: release})
				if err != nil {
					t.Fatal(err)
				}
				err = mirror.New(bus, SyncTopicInterest, nil).Publish(context.Background(), mirror.Envelope{Key: remoteInterestReplicaKey(base), Version: base.ExpiresAt, Op: op, Payload: raw})
				lease, exists := m.snapshots.interests.entries[seed.Key][seed.ConsumerSID]
				if !exists || lease.generation != seed.Generation || lease.expiresAt != seed.ExpiresAt || m.snapshots.interests.total != 1 {
					t.Errorf("refused message changed lease: %+v exists=%v total=%d", lease, exists, m.snapshots.interests.total)
				}
				if err == nil {
					t.Error("interest identity mismatch returned success")
				}
				// 拒绝之后仍能按发布端正式格式续租、撤销；不是关掉全部接收。
				if err := m.snapshots.transport.PublishRemoteInterest(context.Background(), payload, false); err != nil {
					t.Fatal(err)
				}
				if !m.snapshots.interests.interested(payload.Key) {
					t.Fatal("honest renewal missing")
				}
				if err := m.snapshots.transport.PublishRemoteInterest(context.Background(), payload, true); err != nil {
					t.Fatal(err)
				}
				if m.snapshots.interests.interested(payload.Key) {
					t.Fatal("honest release missing")
				}
			})
		}
	}
}

func TestInterestWireRejectsLegacyAndPreservesOrdering(t *testing.T) {
	m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	bus := &interestIdentityBus{}
	_, rep := m.BindSync(bus)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	defer rep.Stop()
	i := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9351), ConsumerSID: 7, ExpiresAt: time.Now().Add(time.Hour).UnixNano()}
	ctx := context.Background()
	// 无代际消息必须拒绝，不能修改兴趣表。
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, false); err == nil {
		t.Fatal("publisher accepted generation zero renewal")
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("legacy renewal accepted")
	}
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, true); err == nil {
		t.Fatal("publisher accepted generation zero release")
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("legacy message created lease")
	}
	// 绕过发布端校验，直接传旧线格式，证明接收端也拒绝。
	for _, release := range []bool{false, true} {
		raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: i, Release: release})
		if err != nil {
			t.Fatal(err)
		}
		err = mirror.New(bus, SyncTopicInterest, nil).Publish(ctx, mirror.Envelope{
			Key: remoteInterestReplicaKey(i), Version: i.ExpiresAt, Op: mirror.OpUpsert, Payload: raw,
		})
		if err == nil || m.snapshots.interests.total != 0 {
			t.Fatalf("legacy wire accepted: err=%v total=%d", err, m.snapshots.interests.total)
		}
	}
	i.Generation = 12
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, false); err != nil {
		t.Fatal(err)
	}
	stale := i
	stale.Generation = 11
	stale.ExpiresAt = time.Now().UnixNano()
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, stale, true); err != nil {
		t.Fatal(err)
	}
	if !m.snapshots.interests.interested(i.Key) {
		t.Fatal("stale wire release canceled newer lease")
	}
	// 无 payload Delete 没有完整身份，明确拒绝且不影响现有租约。
	if err := mirror.New(bus, SyncTopicInterest, nil).PublishDelete(ctx, remoteInterestReplicaKey(i), i.ExpiresAt); err == nil {
		t.Fatal("receiver accepted identity-free delete")
	}
	if !m.snapshots.interests.interested(i.Key) {
		t.Fatal("identity-free delete changed registry")
	}
	i.Generation = 13
	i.ExpiresAt = time.Now().UnixNano()
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, true); err != nil {
		t.Fatal(err)
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("current generation release missing")
	}
}

// 同步传输夹具只递送消息；编解码和接收校验使用正式 Replicator。
type interestIdentityBus struct{ sub *fsyncbus.Subscription }

func (b *interestIdentityBus) Publish(msg *fsyncbus.SyncMsg) error {
	return b.sub.Deliver(context.Background(), msg)
}
func (b *interestIdentityBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.sub = fsyncbus.NewSubscription(topic, h, nil)
	return b.sub, nil
}
