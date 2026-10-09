package activity

import (
	"context"
	"errors"
	"fmt"
	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"os"
	"strings"

	"testing"
	"time"
)

func TestBugfix7ActivityClusterRecoversPartialCompletion(t *testing.T) {
	m, err := bugfix7ClusterService(t, fmt.Sprintf("bugfix7:{activity-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := activityKey("cluster-review")
	base := m.cfg
	m.cfg.Dispatches = bugfix7FailSecondDispatch{base.Dispatches}
	if _, err := m.OpenActivity(ctx, k, []int32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyPhase(ctx, k, 1); err != nil {
		t.Fatal(err)
	}
	complete, err := m.NotifyPhase(ctx, k, 2)
	if !errors.Is(err, context.DeadlineExceeded) || complete.Status != StatusComplete {
		t.Fatalf("partial completion: %+v %v", complete, err)
	}
	first, found, err := m.LookupDispatch(ctx, k, 1)
	if err != nil || !found {
		t.Fatalf("first durable dispatch: %+v %v", first, err)
	}
	// A new service object over the same Redis repairs the complete aggregate.
	restarted, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AdvanceExpired(ctx, k.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int32{1, 2} {
		d, found, err := restarted.LookupDispatch(ctx, k, sid)
		if err != nil || !found {
			t.Fatalf("restored %d: %+v %v", sid, d, err)
		}
		if sid == 1 && d.Token != first.Token {
			t.Fatal("recovery replaced the durable token")
		}
		if owed, err := restarted.OwedDispatches(ctx, k.GroupID, sid, 10); err != nil || len(owed) != 1 {
			t.Fatalf("owed %d: %v %v", sid, owed, err)
		}
		if _, err := restarted.AckDispatch(ctx, k, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
}

type bugfix7FailSecondDispatch struct {
	versionstore.Store[DispatchKey, Dispatch]
}

func (s bugfix7FailSecondDispatch) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	if key.GameSID == 2 {
		return versionstore.Versioned[Dispatch]{}, false, context.DeadlineExceeded
	}
	return s.Store.Create(ctx, key, value)
}

// 存储恢复故障留在领域包；Wiring 的真实集群生命周期由 TaggedClusterLifecycle 单独验证。
func bugfix7ClusterService(t *testing.T, prefix string) (*Service, error) {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addr == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := redis.DefaultConfig("")
	cfg.ClusterAddrs = strings.Split(addr, ",")
	client, err := driver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	stores, err := NewRedisStores(client, prefix, time.Hour)
	if err != nil {
		return nil, err
	}
	key := activityKey("cluster-review")
	t.Cleanup(func() {
		_, _ = client.Del(context.Background(), prefix+":win:"+key.GroupID, prefix+":act:"+key.String(), prefix+":disp:"+(DispatchKey{Activity: key, GameSID: 1}).String(), prefix+":disp:"+(DispatchKey{Activity: key, GameSID: 2}).String(), OwedDispatchKey(prefix, key.GroupID, 1), OwedDispatchKey(prefix, key.GroupID, 2))
	})
	return New(Config{Groups: testGroups(t), Activities: stores.Activities, Participants: stores.Participants, Ledger: stores.Ledger, Audits: stores.Audits, Dispatches: stores.Dispatches, Windows: stores.Windows, ReservationTTL: time.Hour})
}
