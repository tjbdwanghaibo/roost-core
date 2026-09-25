package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

type grantTestAuthority struct {
	WriteAuthority
	grant func() (WriteGrant, error)
}

func (a grantTestAuthority) GrantWrite(context.Context, int64, string, int32) (WriteGrant, error) {
	return a.grant()
}

type authorityLockRedis struct {
	*unlockEvalStub
	loseBeforeRefresh bool
}

func (r *authorityLockRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedRefreshLua && script != versionedAbandonLua {
		return r.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if script == versionedRefreshLua && r.loseBeforeRefresh {
		r.hash["owner"] = "replacement"
	}
	if r.hash["owner"] != args[0].(string) {
		return int64(0), nil
	}
	if script == versionedAbandonLua {
		delete(r.hash, "owner")
	}
	return int64(1), nil
}
func TestAuthorityLockUsesDurableVersionAndCleansFailedAdmission(t *testing.T) {
	for _, mode := range []string{"success", "authority unavailable", "redis replaced"} {
		t.Run(mode, func(t *testing.T) {
			r := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub(), loseBeforeRefresh: mode == "redis replaced"}
			r.hash["version"] = "1"
			unavailable := errors.New("authority unavailable")
			a := grantTestAuthority{grant: func() (WriteGrant, error) {
				if mode == "authority unavailable" {
					return WriteGrant{}, unavailable
				}
				return WriteGrant{Version: 8, Fence: 123}, nil
			}}
			lock := NewVersionedLockFactory(r, a).NewVersionedLock(101, fredis.VersionedLockOptions{TTL: time.Second}).(*versionedLock)
			err := lock.TryLock(context.Background())
			switch mode {
			case "success":
				if err != nil || lock.Version() != 8 || lock.Fence() != 123 || !lock.IsAcquired() {
					t.Fatalf("lock=%+v err=%v", lock, err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			case "authority unavailable":
				if !errors.Is(err, unavailable) || lock.IsAcquired() || r.hash["owner"] != "" || r.hash["version"] != "1" {
					t.Fatalf("err=%v hash=%v", err, r.hash)
				}
			case "redis replaced":
				if !errors.Is(err, ErrVersionedLockExpired) || lock.IsAcquired() || r.hash["owner"] != "replacement" {
					t.Fatalf("err=%v hash=%v", err, r.hash)
				}
			}
		})
	}
}

func TestAuthorityLockRetriesKnownLeaseExpiryBeforeBusinessAdmission(t *testing.T) {
	r := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub()}
	grants := 0
	a := grantTestAuthority{grant: func() (WriteGrant, error) {
		grants++
		if grants == 1 {
			r.mu.Lock()
			delete(r.hash, "owner")
			r.mu.Unlock()
		}
		return WriteGrant{Version: 8, Fence: uint64(grants)}, nil
	}}
	lock := NewVersionedLockFactory(r, a).NewVersionedLock(101, fredis.VersionedLockOptions{TTL: time.Second, RetryCount: 1, RetryInterval: time.Millisecond}).(*versionedLock)
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if grants != 2 || lock.Fence() != 2 || !lock.IsAcquired() {
		t.Fatalf("grants=%d fence=%d held=%v", grants, lock.Fence(), lock.IsAcquired())
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
