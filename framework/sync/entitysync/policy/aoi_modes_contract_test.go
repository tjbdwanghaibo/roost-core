// Black-box tests: the index choice is configuration, not a change to the event or transaction contract.
package policy_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

var consumerModes = []policy.AOIIndexMode{policy.IndexBlock, policy.IndexCrossList, policy.IndexKDTree}

func xy(x, y int64) spatial.Point { return spatial.Point{X: x, Y: y} }

func modeConfig(mode policy.AOIIndexMode) policy.AOIConfig {
	return policy.AOIConfig{IndexMode: mode, Bounds: spatial.Rect{Min: xy(-64, -64), Max: xy(64, 64)}, BlockSize: 8,
		EnterRadius: 8, LeaveRadius: 10, MaxLeaveRadius: 18, LODs: [][]int64{{3, 6}, {5}}}
}

func mustOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func enter(o, s int64, band int) policy.InterestEvent {
	return policy.InterestEvent{Observer: o, Subject: s, Kind: policy.InterestEnter, Band: band}
}
func leave(o, s int64) policy.InterestEvent {
	return policy.InterestEvent{Observer: o, Subject: s, Kind: policy.InterestLeave, Band: -1}
}
func bandChange(o, s int64, band int) policy.InterestEvent {
	return policy.InterestEvent{Observer: o, Subject: s, Kind: policy.InterestBandChanged, Band: band}
}
func checkEvents(t testing.TB, tick string, got []policy.InterestEvent, want ...policy.InterestEvent) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: ordered events=%v, want %v", tick, got, want)
	}
}

func TestAOIModesConsumerTimeline(t *testing.T) {
	for _, mode := range consumerModes {
		t.Run(string(mode), func(t *testing.T) {
			a, err := policy.NewAOI(modeConfig(mode))
			mustOK(t, err)
			// These are expected states derived only from the hand-written events, never from an index.
			members := map[int64][]int64{101: nil, 102: nil}
			tick := func(name string, coverage []policy.InterestEvent, want ...policy.InterestEvent) {
				t.Helper()
				checkEvents(t, name+"/distance", a.Flush(), want...)
				checkEvents(t, name+"/coverage", a.FlushBlocks(), coverage...)
				for _, e := range want {
					ids := members[e.Observer]
					i, ok := slices.BinarySearch(ids, e.Subject)
					switch e.Kind {
					case policy.InterestEnter:
						if ok {
							t.Fatal("duplicate expected enter")
						}
						ids = slices.Insert(ids, i, e.Subject)
					case policy.InterestLeave:
						if !ok {
							t.Fatal("unexpected expected leave")
						}
						ids = slices.Delete(ids, i, i+1)
					}
					members[e.Observer] = ids
				}
				for _, o := range []int64{101, 102} {
					if got := a.Visible(o); !slices.Equal(got, members[o]) {
						t.Fatalf("%s Visible(%d)=%v want %v", name, o, got, members[o])
					}
				}
			}
			mustOK(t, a.AddSubject(7, xy(0, 0)))
			mustOK(t, a.AddSubject(8, xy(9, 0)))
			mustOK(t, a.AddSubject(9, xy(63, 63)))
			mustOK(t, a.AddObserver(101, 7, xy(0, 0), policy.ObserverView{}))
			mustOK(t, a.AddObserver(102, 0, xy(16, 0), policy.ObserverView{}))
			tick("outside-enter/self-exclusion", nil, enter(102, 8, 2))
			move := func(x int64, want ...policy.InterestEvent) {
				t.Helper()
				mustOK(t, a.MoveSubject(8, xy(x, 0)))
				tick(fmt.Sprintf("x=%d", x), nil, want...)
			}
			move(8, enter(101, 8, 2))
			move(6, bandChange(101, 8, 1))
			move(5, leave(102, 8))
			move(3, bandChange(101, 8, 0))
			move(10, bandChange(101, 8, 2), enter(102, 8, 1))
			move(11, leave(101, 8))
			v := policy.ObserverView{LOD: 1, Radius: 16}
			got, err := a.SetObserverView(101, v)
			mustOK(t, err)
			if got != v {
				t.Fatalf("view=%v want %v", got, v)
			}
			tick("expand", nil, enter(101, 8, 1))
			got, err = a.SetObserverView(101, policy.ObserverView{Radius: 16})
			mustOK(t, err)
			if got.LOD != 0 {
				t.Fatal(got)
			}
			tick("change LOD only", nil, bandChange(101, 8, 2))
			mustOK(t, a.MoveObserver(101, xy(-12, 0)))
			tick("pan", nil, leave(101, 8))
			left, right := a.BlockAt(xy(-24, 0)), a.BlockAt(xy(24, 0))
			if left < 0 || right < 0 || left == right {
				t.Fatal("two distinct valid blocks required")
			}
			mustOK(t, a.SetObservedBlocks(9, []int64{left, left}))
			tick("add duplicate coverage", []policy.InterestEvent{enter(101, 9, 0)})
			mustOK(t, a.SetObservedBlocks(9, []int64{right}))
			tick("replace coverage", []policy.InterestEvent{leave(101, 9), enter(102, 9, 0)})
			mustOK(t, a.SetObservedBlocks(9, nil))
			tick("clear coverage", []policy.InterestEvent{leave(102, 9)})
			mustOK(t, a.SetObservedBlocks(9, []int64{right}))
			tick("restore coverage", []policy.InterestEvent{enter(102, 9, 0)})
			mustOK(t, a.RemoveSubject(9))
			mustOK(t, a.AddSubject(9, xy(-64, -64)))
			tick("new lifetime has no coverage", []policy.InterestEvent{leave(102, 9)})
			mustOK(t, a.RemoveSubject(8))
			mustOK(t, a.AddSubject(8, xy(17, 0)))
			tick("same pair leave before re-enter", nil, leave(102, 8), enter(102, 8, 0))
			if a.BlockAt(xy(-64, -64)) < 0 || a.BlockAt(xy(63, 63)) < 0 {
				t.Fatal("lost included corners")
			}
			for _, p := range []spatial.Point{xy(64, 0), xy(0, 64), xy(-65, 0), xy(0, -65)} {
				if a.BlockAt(p) != -1 || !errors.Is(a.MoveSubject(8, p), spatial.ErrInvalidBounds) || !errors.Is(a.MoveObserver(102, p), spatial.ErrInvalidBounds) {
					t.Fatalf("accepted excluded point %v", p)
				}
			}
			tick("rejected writes are atomic", nil)
			mustOK(t, a.RemoveObserver(102))
			mustOK(t, a.AddObserver(102, 8, xy(17, 0), policy.ObserverView{}))
			tick("observer lifetime changes self", nil, leave(102, 8))
		})
	}
}

func TestAOIModesConsumerCapacity(t *testing.T) {
	for _, mode := range consumerModes {
		t.Run(string(mode), func(t *testing.T) {
			cfg := modeConfig(mode)
			cfg.MaxVisible = 2
			a, err := policy.NewAOI(cfg)
			mustOK(t, err)
			for _, s := range []struct {
				id int64
				p  spatial.Point
			}{{19, xy(0, -5)}, {11, xy(0, 5)}, {-3, xy(-5, 0)}} {
				mustOK(t, a.AddSubject(s.id, s.p))
			}
			check := func(name string, ids []int64, want ...policy.InterestEvent) {
				t.Helper()
				checkEvents(t, name, a.Flush(), want...)
				if got := a.Visible(22); !slices.Equal(got, ids) {
					t.Fatalf("%s: visible=%v want %v", name, got, ids)
				}
			}
			mustOK(t, a.AddObserver(22, 0, xy(0, 0), policy.ObserverView{}))
			check("equal-distance cold admission", []int64{-3, 11}, enter(22, -3, 1), enter(22, 11, 1))
			mustOK(t, a.AddSubject(-9, xy(5, 0)))
			check("equal-distance newcomer keeps incumbents", []int64{-3, 11})
			mustOK(t, a.MoveSubject(19, xy(0, -4)))
			check("strictly closer evicts smaller-ID farthest", []int64{11, 19}, leave(22, -3), enter(22, 19, 1))
			mustOK(t, a.SetObserverLimit(22, 0, true))
			check("zero cap", nil, leave(22, 11), leave(22, 19))
			mustOK(t, a.SetObserverLimit(22, 0, false))
			check("restore default cap", []int64{-9, 19}, enter(22, -9, 1), enter(22, 19, 1))
		})
	}
}

func TestAOIModesConsumerClusterSeam(t *testing.T) {
	for _, mode := range consumerModes {
		t.Run(string(mode), func(t *testing.T) {
			c, err := policy.NewAOICluster(modeConfig(mode))
			mustOK(t, err)
			mustOK(t, c.AddArea(1, spatial.Rect{Min: xy(-64, -64), Max: xy(0, 64)}))
			mustOK(t, c.AddArea(2, spatial.Rect{Min: xy(0, -64), Max: xy(64, 64)}))
			mustOK(t, c.AddObserver(1, 0, xy(-1, 0), policy.ObserverView{}))
			mustOK(t, c.AddSubject(2, xy(-2, 0)))
			checkEvents(t, "entry", c.Flush(), enter(1, 2, 0))
			mustOK(t, c.MoveSubject(2, xy(0, 0)))
			mustOK(t, c.MoveObserver(1, xy(0, 0)))
			checkEvents(t, "half-open seam, no blink", c.Flush())
			if !slices.Equal(c.Visible(1), []int64{2}) {
				t.Fatal(c.Visible(1))
			}
			if !errors.Is(c.MoveSubject(2, xy(64, 0)), policy.ErrAreaUnknown) {
				t.Fatal("exclusive outer edge has no owner")
			}
			mustOK(t, c.MoveSubject(2, xy(11, 0)))
			checkEvents(t, "leave", c.Flush(), leave(1, 2))
			if len(c.Visible(1)) != 0 {
				t.Fatal(c.Visible(1))
			}
		})
	}
}

type modeTransport struct{}

func (modeTransport) Push(context.Context, entitysync.SessionID, []byte) error { return nil }

type modeEntity struct{ *entity.EntityBase }

func (e *modeEntity) Base() *entity.EntityBase { return e.EntityBase }

// Followers move in the same committed batch as the target: four dirty moving views exercise
// simultaneous full queries, not concurrent writes to a non-thread-safe stand-alone AOI.
func TestAOIModesConsumerInterestBatch(t *testing.T) {
	for _, mode := range consumerModes {
		for _, workers := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/workers=%d", mode, workers), func(t *testing.T) {
				ctx := context.Background()
				m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: modeTransport{}})
				mustOK(t, err)
				t.Cleanup(func() { _ = m.Close(ctx) })
				in, err := policy.NewInterest(policy.InterestConfig{Manager: m, AOI: modeConfig(mode), Workers: workers, ApplyMode: policy.ApplySync})
				mustOK(t, err)
				t.Cleanup(in.Close)
				var es []entity.IThreadSafeEntity
				for i, id := range []int64{10, 11, 12, 13, 50} {
					state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "aoi-mode", Packer: entity.SubjectSyncPackFunc{
						Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
							return entity.TakeFrozenSyncPayload(1, []byte{1}), nil
						},
						Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
							return entity.TakeFrozenSyncPayload(1, []byte{2}), nil
						}}})
					mustOK(t, m.Register(state))
					e := &modeEntity{entity.NewEntityBase(id, entity.EntityCategory(4), true)}
					e.SetSyncState(state)
					es = append(es, e)
					if id == 50 {
						mustOK(t, in.Show(id, xy(60, 60)))
					} else {
						player := int64(100 + i)
						mustOK(t, m.OpenSession(entitysync.SessionID(player)))
						mustOK(t, in.Enter(player, id, xy(0, int64(i)), policy.ObserverView{}))
					}
				}
				check := func(stage string, distance, held bool, blocks []int64) {
					t.Helper()
					mustOK(t, m.Flush(ctx))
					if got := in.ObservedBlocks(50); !slices.Equal(got, blocks) {
						t.Fatalf("%s coverage=%v want %v", stage, got, blocks)
					}
					for i := int64(0); i < 4; i++ {
						want := []int64{10, 11, 12, 13}
						visible := slices.Delete(slices.Clone(want), int(i), int(i)+1)
						if distance {
							visible = append(visible, 50)
						}
						if held {
							want = append(want, 50)
						}
						if got := in.Visible(100 + i); !slices.Equal(got, visible) {
							t.Fatalf("%s view %d=%v want %v", stage, i, got, visible)
						}
						if got := in.Seen(100 + i); !slices.Equal(got, want) {
							t.Fatalf("%s held %d=%v want %v", stage, i, got, want)
						}
					}
					var subs []entitysync.SessionID
					if held {
						subs = []entitysync.SessionID{100, 101, 102, 103}
					}
					if got := m.Subscribers(50); !slices.Equal(got, subs) {
						t.Fatalf("%s subscribers=%v want %v", stage, got, subs)
					}
				}
				check("initial", false, false, nil)
				old := []int64{in.BlockAt(xy(0, 0))}
				for _, outcome := range []string{"rollback", "reject", "commit"} {
					tx := entity.BeginSyncMutation(es, m)
					for i := 0; i < 4; i++ {
						mustOK(t, in.QueueMove(es[i], xy(32, int64(i))))
					}
					mustOK(t, in.QueueSpatial(es[4], xy(33, 0), old))
					check(outcome+" pending", false, false, nil)
					switch outcome {
					case "rollback":
						tx.Finish(false)
					case "reject":
						tx.Admit()
						tx.RejectEntities([]int64{10, 11, 12, 13, 50})
					case "commit":
						tx.Admit()
						tx.Confirm()
					}
					check(outcome+" before release", false, false, nil)
					tx.Release()
					if outcome == "commit" {
						check(outcome, true, true, old)
					} else {
						check(outcome, false, false, nil)
					}
				}
				near := []int64{in.BlockAt(xy(32, 0))}
				mustOK(t, in.QueueSpatial(es[4], xy(60, 60), near))
				check("coverage replaces distance", false, true, near)
				mustOK(t, in.QueueObservedBlocks(es[4], nil))
				check("last source released", false, false, nil)
				tx := entity.BeginSyncMutation(es[4:], m)
				mustOK(t, in.QueueSpatial(es[4], xy(33, 0), near))
				tx.Admit()
				tx.Release()
				mustOK(t, in.Hide(50))
				mustOK(t, in.Show(50, xy(60, 60)))
				check("replace before confirmation", false, false, nil)
				tx.Confirm()
				check("stale generation discarded", false, false, nil)
			})
		}
	}
}
