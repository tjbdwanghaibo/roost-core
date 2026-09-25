//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

var registerRedisOwnershipKind = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 237, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
})

func TestRealRemoteOwnershipClaimAndTransfer(t *testing.T) {
	firstRedis, secondRedis := realRemoteRedis(t), realRemoteRedis(t)
	key := remoteRedisKey(t, firstRedis)
	registerRedisOwnershipKind()
	const kind entity.EntityKind = 237
	id := testRemoteFullIDWithKind(7211, 1, kind)
	cfg := DefaultConfig()
	cfg.LockKey = key
	cfg.LockTTL = time.Second
	cfg.OpTimeout = time.Second
	lockKey := fmt.Sprintf("lock:%s:%d", key, id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := firstRedis.Del(ctx, lockKey, lockKey+":fence"); err != nil {
			t.Errorf("cleanup lock: %v", err)
		}
	})
	managers := []*Manager{NewManager(NewVersionedLockFactory(firstRedis), cfg, 1000), NewManager(NewVersionedLockFactory(secondRedis), cfg, 2000)}
	managers[0].SetOwnershipStore(NewRedisMarker(firstRedis, key))
	managers[1].SetOwnershipStore(NewRedisMarker(secondRedis, key))
	for _, m := range managers {
		backend := newRemoteTestLoader()
		backend.add(newTestRemoteEntity(7211, 1, kind))
		m.SetBackend(backend)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type claim struct {
		index int
		err   error
	}
	results := make(chan claim, 2)
	for i, m := range managers {
		go func() { _, err := m.ClaimRemoteOwnership(ctx, id); results <- claim{i, err} }()
	}
	winner := -1
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner >= 0 {
				t.Fatal("two owners claimed")
			}
			winner = result.index
		} else if !errors.Is(result.err, entity.ErrRemoteFenced) {
			t.Fatal(result.err)
		}
	}
	if winner < 0 {
		t.Fatal("no owner claimed")
	}
	old, next := managers[winner], managers[1-winner]
	shared, err := old.EnterRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := old.TransferRemoteOwnership(ctx, id, next.localSid)
	if err != nil {
		t.Fatal(err)
	}
	if !transferred.Shared || transferred.OwnerSid != next.localSid || transferred.MarkerEpoch <= shared.MarkerEpoch || transferred.RouteEpoch <= shared.RouteEpoch {
		t.Fatalf("shared=%+v transferred=%+v", shared, transferred)
	}
	if _, err := old.LeaveRemoteSharedMode(ctx, id); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("old owner still writes ownership: %v", err)
	}
	local, err := next.LeaveRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if local.Shared || local.MarkerEpoch <= transferred.MarkerEpoch {
		t.Fatalf("local=%+v", local)
	}
	actual, found, err := old.GetRemoteOwnership(ctx, id)
	if err != nil || !found || actual != local {
		t.Fatalf("old reader sees=%+v found=%v err=%v", actual, found, err)
	}
}
