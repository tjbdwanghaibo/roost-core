package activity

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func bugfix5Activity(t *testing.T, mutate func(*Config)) (*Service, *activityClock) {
	t.Helper()
	return newActivityService(t, func(c *Config) {
		if os.Getenv("ROOST_BUGFIX5_BACKEND") == "redis" {
			addr := os.Getenv("ROOST_REVIEW_REDIS")
			if addr == "" {
				t.Fatal("isolated Redis address required")
			}
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			stores, err := NewRedisStores(client, fmt.Sprintf("bugfix5:activity:%d", time.Now().UnixNano()), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			c.Activities = stores.Activities
			c.Participants = stores.Participants
			c.Ledger = stores.Ledger
			c.Audits = stores.Audits
			c.Dispatches = stores.Dispatches
			c.Windows = stores.Windows
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

type bugfix5SlowCreate struct {
	versionstore.Store[Key, Activity]
	once             atomic.Bool
	key              Key
	entered, release chan struct{}
}

func (s *bugfix5SlowCreate) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	if key == s.key && s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Create(ctx, key, value)
}

func TestBugfix5LateCreateCannotBorrowAnAlreadyPromisedSlot(t *testing.T) {
	key := activityKey("zz-late")
	var gate *bugfix5SlowCreate
	s, clock := bugfix5Activity(t, func(c *Config) {
		gate = &bugfix5SlowCreate{Store: c.Activities, key: key, entered: make(chan struct{}), release: make(chan struct{})}
		c.Activities = gate
	})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { _, err := s.OpenActivity(ctx, key, []int32{1, 2}); done <- err }()
	<-gate.entered
	released := false
	defer func() {
		if !released {
			close(gate.release)
			<-done
		}
	}()
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPendingActivities-1; i++ {
		openActivity(t, s, activityKey(fmt.Sprintf("new-%d", i)), 1)
	}
	if _, err := s.OpenActivity(ctx, activityKey("one-too-many"), []int32{1}); !errors.Is(err, ErrBacklog) {
		t.Fatal(err)
	}
	close(gate.release)
	released = true
	if err := <-done; err != nil && !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || window.Value.pending() != MaxPendingActivities {
		t.Fatal(window, err)
	}
	if _, err := s.NotifyPhase(ctx, key, 1); err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.GraceWindow + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LookupActivity(ctx, key)
	if err != nil || !found || got.Status != StatusComplete {
		t.Fatal(got, found, err)
	}
}

type bugfix5LostAdmission struct {
	versionstore.Store[string, Window]
	writes atomic.Int32
	failAt int32
}

func (s *bugfix5LostAdmission) Update(ctx context.Context, key string, mutate versionstore.Mutate[Window]) (versionstore.Versioned[Window], bool, error) {
	got, applied, err := s.Store.Update(ctx, key, mutate)
	if err == nil && applied && s.writes.Add(1) == s.failAt {
		return versionstore.Versioned[Window]{}, false, errors.New("admission response lost")
	}
	return got, applied, err
}
func TestBugfix5LostAdmissionReplyRecoversItsOriginalPlan(t *testing.T) {
	s, clock := bugfix5Activity(t, func(c *Config) { c.Windows = &bugfix5LostAdmission{Store: c.Windows, failAt: 1} })
	ctx := context.Background()
	key := activityKey("lost")
	expected := []int32{1, 2}
	if _, err := s.OpenActivity(ctx, key, expected); err == nil {
		t.Fatal("lost reply not reported")
	}
	expected[0] = 999
	window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || len(window.Value.Opening) != 1 || window.Value.Opening[0].Intent == nil {
		t.Fatal(window, err)
	}
	rebuilt, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	got, found, err := rebuilt.LookupActivity(ctx, key)
	if err != nil || !found || len(got.ExpectedGameSIDs) != 2 || got.ExpectedGameSIDs[0] != 1 {
		t.Fatal(got, found, err)
	}
	if _, err := rebuilt.OpenActivity(ctx, key, []int32{3}); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	got, _, err = rebuilt.LookupActivity(ctx, key)
	if err != nil || got.ExpectedGameSIDs[0] != 1 {
		t.Fatal(got, err)
	}
}

type bugfix5LostCreation struct {
	versionstore.Store[Key, Activity]
	once atomic.Bool
}

func (s *bugfix5LostCreation) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	stored, created, err := s.Store.Create(ctx, key, value)
	if err == nil && created && s.once.CompareAndSwap(false, true) {
		return versionstore.Versioned[Activity]{}, false, errors.New("creation response lost")
	}
	return stored, created, err
}

func TestBugfix5CreatedOrConfirmedReplyLossRemainsRecoverable(t *testing.T) {
	for _, failure := range []string{"create", "confirm"} {
		t.Run(failure, func(t *testing.T) {
			s, clock := bugfix5Activity(t, func(c *Config) {
				if failure == "create" {
					c.Activities = &bugfix5LostCreation{Store: c.Activities}
				} else {
					c.Windows = &bugfix5LostAdmission{Store: c.Windows, failAt: 2}
				}
			})
			ctx := context.Background()
			key := activityKey(failure)
			if _, err := s.OpenActivity(ctx, key, []int32{1, 2}); err == nil {
				t.Fatal("lost response not reported")
			}
			window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || !window.Value.contains(key) || window.Value.pending() != 1 {
				t.Fatal(window, err)
			}
			rebuilt, err := New(s.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := rebuilt.NotifyPhase(ctx, key, 1); err != nil {
				t.Fatal(err)
			}
			clock.advance(s.cfg.GraceWindow + time.Second)
			advanced, err := rebuilt.AdvanceExpired(ctx, key.GroupID, 1)
			if err != nil || len(advanced) != 1 || advanced[0].Status != StatusComplete {
				t.Fatal(advanced, err)
			}
			window, _, err = rebuilt.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || window.Value.pending() != 0 || !window.Value.delivering(key) {
				t.Fatal(window, err)
			}
		})
	}
}

type bugfix5CountingActivities struct {
	versionstore.Store[Key, Activity]
	reads atomic.Int64
}

func (s *bugfix5CountingActivities) Get(ctx context.Context, key Key) (versionstore.Versioned[Activity], bool, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, key)
}
func TestBugfix5OversizedLegacyWindowRotatesAcrossServiceRebuild(t *testing.T) {
	var store *bugfix5CountingActivities
	s, clock := bugfix5Activity(t, func(c *Config) { store = &bugfix5CountingActivities{Store: c.Activities}; c.Activities = store })
	ctx := context.Background()
	window := Window{GroupID: "group-a"}
	for i := 0; i < MaxPendingActivities+1; i++ {
		key := activityKey(fmt.Sprintf("legacy-%03d", i))
		window.Keys = append(window.Keys, key)
		value := Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1, 2}}
		if _, _, err := store.Create(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.cfg.Windows.Create(ctx, window.GroupID, window); err != nil {
		t.Fatal(err)
	}
	tail := window.Keys[len(window.Keys)-1]
	if _, err := s.NotifyPhase(ctx, tail, 1); err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.GraceWindow + time.Second)
	store.reads.Store(0)
	if _, err := s.AdvanceExpired(ctx, window.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	if store.reads.Load() > MaxPendingActivities {
		t.Fatal("unbounded reads", store.reads.Load())
	}
	rebuilt, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.reads.Store(0)
	if _, err := rebuilt.AdvanceExpired(ctx, window.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	if store.reads.Load() > MaxPendingActivities {
		t.Fatal("unbounded reads", store.reads.Load())
	}
	got, found, err := rebuilt.LookupActivity(ctx, tail)
	if err != nil || !found || got.Status != StatusComplete {
		t.Fatal(got, found, err)
	}
}

func TestBugfix5LegacyOpeningNeedsAPlanInsteadOfTimeoutReclaim(t *testing.T) {
	s, clock := bugfix5Activity(t, nil)
	ctx := context.Background()
	key := activityKey("legacy-opening")
	if _, _, err := s.cfg.Windows.Create(ctx, key.GroupID, Window{GroupID: key.GroupID, Opening: []OpeningEntry{{Key: key, AdmittedAtUnix: clock.Now().Unix()}}}); err != nil {
		t.Fatal(err)
	}
	clock.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, key.GroupID, 1); err != nil {
		t.Fatal(err)
	}
	window, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || window.Value.pending() != 1 {
		t.Fatal(window, err)
	}
	if _, err := s.OpenActivity(ctx, key, []int32{1, 2}); err != nil {
		t.Fatal(err)
	}
	window, _, err = s.cfg.Windows.Get(ctx, key.GroupID)
	if err != nil || len(window.Value.Opening) != 0 || len(window.Value.Keys) != 1 {
		t.Fatal(window, err)
	}
}
