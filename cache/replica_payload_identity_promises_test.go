package cache

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// RR-20261005-NC-33：信封路由身份必须与最终写入的缓存对象一致。
// 旧接收端只反序列化就 Set；外层 key/version 正确仍可写入另一个键或版本。
func TestReplicaPayloadIdentityBeforeStoreMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value testItem
	}{
		{"key", testItem{ID: 8, Version: 3, Data: "bad"}},
		{"version", testItem{ID: 7, Version: 4, Data: "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			bus := newFakeSyncBus()
			store := NewLocalStore[int64, testItem](testItemConfig())
			for _, id := range []int64{7, 8} {
				if err := store.Set(ctx, testItem{ID: id, Version: 2, Data: "before"}); err != nil {
					t.Fatal(err)
				}
			}
			s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "identity", KeyOf: func(v testItem) int64 { return v.ID }, VersionOf: func(v testItem) int64 { return v.Version }})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Stop(context.Background()) }()
			raw, err := mirror.MarshalPayload(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			// 通过真实 Replicator：SyncMsg 与 Envelope 两层身份完全一致。
			err = mirror.New(bus, "identity", nil).Publish(ctx, mirror.Envelope{Key: 7, Version: 3, Payload: raw})
			for _, id := range []int64{7, 8} {
				v, ok, readErr := store.Get(ctx, id)
				if readErr != nil || !ok || v.Data != "before" || v.Version != 2 {
					t.Errorf("refused message changed key %d: %+v ok=%v err=%v", id, v, ok, readErr)
				}
			}
			if err == nil {
				t.Error("payload identity mismatch returned success")
			}
			if err := s.Publish(ctx, testItem{ID: 7, Version: 3, Data: "after"}); err != nil {
				t.Fatal(err)
			}
			v, ok, err := store.Get(ctx, 7)
			if err != nil || !ok || v.Data != "after" {
				t.Fatalf("honest recovery: %+v %v %v", v, ok, err)
			}
		})
	}
}

func TestReplicaOptionalVersionAndDeleteRemainUsable(t *testing.T) {
	ctx := context.Background()
	bus := newFakeSyncBus()
	store := NewLocalStore[int64, testItem](testItemConfig())
	s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "optional", KeyOf: func(v testItem) int64 { return v.ID }, DeleteKeyOf: func(id int64) int64 { return id }})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	if err := s.Publish(ctx, testItem{ID: 7, Version: 99}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, 7); err != nil || !ok {
		t.Fatalf("unversioned config: %v %v", ok, err)
	}
	if err := s.PublishDelete(ctx, 7, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, 7); err != nil || ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}

// 写前身份校验不能绕过原 Store 对 nil 的拒绝而引入 callback panic。
func TestReplicaNullPointerRefusedBeforeIdentityCallback(t *testing.T) {
	store := NewLocalStore[int64, *testItem](StoreConfig[int64, *testItem]{
		KeyOf: func(v *testItem) int64 { return v.ID },
		ValidateValue: func(v *testItem) error {
			if v == nil {
				return ErrInvalidKey
			}
			return nil
		},
	})
	bus := newFakeSyncBus()
	s := NewReplicaSyncer(bus, ReplicaConfig[int64, *testItem]{Store: store, Topic: "pointer", KeyOf: func(v *testItem) int64 { return v.ID }})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	if err := mirror.New(bus, "pointer", nil).Publish(context.Background(), mirror.Envelope{Key: 7, Payload: []byte(" null ")}); err == nil {
		t.Fatal("null pointer must be refused")
	}
	if err := s.Publish(context.Background(), &testItem{ID: 7, Data: "after"}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := store.Get(context.Background(), 7)
	if err != nil || !ok || v.Data != "after" {
		t.Fatalf("pointer recovery: %+v %v %v", v, ok, err)
	}
}
