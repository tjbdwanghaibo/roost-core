package regionsummary_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/gameplay/regionsummary"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const observer = 7
const left, right = 8001, 8002

var world = spatial.Rect{Max: spatial.Point{X: 256, Y: 256}}

func config() regionsummary.Config {
	return regionsummary.Config{View: 70, DetailView: 71, DetailProfile: entity.SyncProfile{Key: "landmark_public"}, Public: func(id int64) bool { return id >= 100 && id < 164 }, Areas: []regionsummary.Area{
		{SubjectID: left, Bounds: spatial.Rect{Min: spatial.Point{X: 64}, Max: spatial.Point{X: 128, Y: 256}}},
		{SubjectID: right, Bounds: spatial.Rect{Min: spatial.Point{X: 128}, Max: spatial.Point{X: 256, Y: 256}}},
	}}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// This consumer handles object operations, not just concatenated payload bytes. A reconnect
// starts with no references and the real Decoder enforces version/generation continuity.
type client struct {
	d      *entitysync.Decoder
	held   map[int64]string
	counts map[int64]uint64
	frames [][]byte
	full   int
}

func (c *client) reset() {
	c.d = entitysync.NewDecoder(frame.DefaultLimits())
	c.held = map[int64]string{}
	c.counts = map[int64]uint64{}
}
func (c *client) push(_ context.Context, _ entitysync.SessionID, data []byte) error {
	f, err := c.d.Decode(data)
	if err != nil {
		return err
	}
	c.frames = append(c.frames, bytes.Clone(data))
	if f.Kind == frame.Full {
		clear(c.held)
		clear(c.counts)
		c.full++
	}
	for _, o := range f.Objects {
		if o.Operation == frame.ObjectRemove {
			delete(c.counts, o.SubjectID)
			delete(c.held, o.SubjectID)
			continue
		}
		for _, component := range o.Components {
			u := component.Update
			c.held[u.SubjectID] = u.Namespace
			if u.Namespace == regionsummary.Namespace {
				if u.SubjectID != left && u.SubjectID != right {
					return fmt.Errorf("source ID %d exposed as marker", u.SubjectID)
				}
				if u.Profile != (entity.SyncProfile{}).Normalize() {
					return fmt.Errorf("unexpected summary profile: %v", u.Profile)
				}
				var count wrapperspb.UInt64Value
				if err := proto.Unmarshal(u.Payload.BytesCopy(), &count); err != nil {
					return err
				}
				if len(count.ProtoReflect().GetUnknown()) != 0 {
					return errors.New("summary exposed undeclared fields")
				}
				if count.Value == 0 {
					return errors.New("empty areas require Remove, not a zero marker")
				}
				c.counts[u.SubjectID] = count.Value
			}
		}
	}
	return nil
}
func (c *client) bytes() int {
	var n int
	for _, f := range c.frames {
		n += len(f)
	}
	return n
}

type scene struct {
	t        *testing.T
	client   client
	manager  *entitysync.Manager
	interest *policy.Interest
	overview *regionsummary.Map
}

func newScene(t *testing.T, enabled bool, options ...func(*policy.InterestConfig)) *scene {
	t.Helper()
	s := &scene{t: t}
	s.client.reset()
	var err error
	s.manager, err = entitysync.NewManager(entitysync.ManagerConfig{Transport: entitysync.TransportFunc(s.client.push)})
	must(t, err)
	t.Cleanup(func() { _ = s.manager.Close(context.Background()) })
	cfg := policy.InterestConfig{Manager: s.manager, AOI: policy.AOIConfig{Bounds: world, BlockSize: 16, EnterRadius: 8, LeaveRadius: 10}}
	for _, opt := range options {
		opt(&cfg)
	}
	s.interest, err = policy.NewInterest(cfg)
	must(t, err)
	t.Cleanup(s.interest.Close)
	if enabled {
		s.overview, err = regionsummary.New(s.manager, s.interest, world, config())
		must(t, err)
		t.Cleanup(s.overview.Close)
	}
	must(t, s.manager.OpenSession(observer))
	must(t, s.interest.Watch(observer, spatial.Point{X: 2, Y: 2}, policy.ObserverView{}))
	return s
}
func (s *scene) show(id int64, at spatial.Point) {
	// Deliberately small per-object control: the same standard protobuf scalar constant in
	// every public landmark. No private coordinates or artificially inflated detail records.
	state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "landmark", Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		p, err := proto.Marshal(wrapperspb.UInt64(1))
		return entity.TakeFrozenSyncPayload(1, p), err
	}}})
	must(s.t, s.manager.Register(state))
	must(s.t, s.interest.Show(id, at))
	if s.overview != nil {
		s.overview.Place(id, at)
	}
}
func (s *scene) move(id int64, x int64) {
	at := spatial.Point{X: x, Y: 80}
	must(s.t, s.interest.Move(id, at))
	if s.overview != nil {
		s.overview.Place(id, at)
	}
}
func (s *scene) hide(id int64) {
	must(s.t, s.interest.Hide(id))
	if s.overview != nil {
		s.overview.Forget(id)
	}
}
func (s *scene) settle() {
	s.t.Helper()
	for range 3 {
		must(s.t, s.manager.Flush(context.Background()))
	}
}
func (s *scene) counts(want map[int64]uint64) {
	s.t.Helper()
	s.settle()
	if !maps.Equal(s.client.counts, want) {
		s.t.Fatalf("client counts: %v want %v", s.client.counts, want)
	}
	// The ordinary near object is kept independently in every mode, solely via this same Interest.
	if s.client.held[50] != "landmark" || len(s.client.held) != 1+len(want) {
		s.t.Fatalf("unexpected near/far objects: %v", s.client.held)
	}
	if got := s.manager.AuditStats().Subscriptions; got != len(s.client.held) {
		s.t.Fatalf("server subscriptions %d != client held %d", got, len(s.client.held))
	}
}

func TestPublicRegionOverviewLifecycle(t *testing.T) {
	s := newScene(t, true)
	s.show(50, spatial.Point{X: 3, Y: 3})
	for _, id := range []int64{100, 101, 999} {
		s.show(id, spatial.Point{X: 100, Y: 80})
	}
	s.counts(nil)
	must(t, s.overview.SetMode(observer, regionsummary.Detail))
	s.settle()
	if len(s.client.held) != 3 || s.client.held[100] != "landmark" || s.client.held[101] != "landmark" {
		t.Fatal("detail membership", s.client.held)
	}
	if !slices.Equal(s.overview.OrdinarySeen(observer), []int64{50}) {
		t.Fatal("detail-only sources leaked into ordinary query")
	}
	must(t, s.overview.SetMode(observer, regionsummary.Count))
	s.counts(map[int64]uint64{left: 2})
	before := s.client.bytes()
	s.move(100, 110)
	s.move(999, 140)
	s.counts(map[int64]uint64{left: 2})
	if s.client.bytes() != before {
		t.Fatal("within-region move or private move emitted a frame")
	}
	s.move(100, 128)
	s.counts(map[int64]uint64{left: 1, right: 1})
	s.move(100, 128)
	s.hide(101)
	s.counts(map[int64]uint64{right: 1})
	must(t, s.overview.SetMode(observer, regionsummary.Detail))
	s.settle()
	if len(s.client.held) != 2 || s.client.held[100] != "landmark" || len(s.client.counts) != 0 {
		t.Fatal("back to public detail", s.client.held)
	}
	must(t, s.overview.SetMode(observer, regionsummary.Off))
	s.counts(nil)
	must(t, s.overview.SetMode(observer, regionsummary.Count))
	s.counts(map[int64]uint64{right: 1})
	must(t, s.interest.Unwatch(observer))
	s.manager.CloseSession(observer)
	s.settle()
	if s.manager.AuditStats().Subscriptions != 0 {
		t.Fatal("connection left stale subscriptions")
	}
	if err := s.overview.SetMode(observer, regionsummary.Count); !errors.Is(err, policy.ErrInterestUnknown) {
		t.Fatalf("non-member: %v", err)
	}
	// Reconnect while the observer was absent: only the current marker remains; no old grant.
	s.move(100, 72)
	s.client.reset()
	full := s.client.full
	must(t, s.manager.OpenSession(observer))
	must(t, s.interest.Watch(observer, spatial.Point{X: 2, Y: 2}, policy.ObserverView{}))
	s.counts(nil)
	if s.client.full != full+1 {
		t.Fatal("new session lacked a fresh Full")
	}
	must(t, s.overview.SetMode(observer, regionsummary.Count))
	s.counts(map[int64]uint64{left: 1})
	s.hide(100)
	s.counts(nil)
	s.show(103, spatial.Point{X: 64, Y: 80})
	s.counts(map[int64]uint64{left: 1})
	s.overview.Close()
	s.counts(nil)
	if s.manager.Stats().Subjects != 5 {
		t.Fatalf("markers leaked after Close: %d registered", s.manager.Stats().Subjects)
	}
}

func TestFirstScreenPublicOverviewControls(t *testing.T) {
	type result struct {
		n, sub, frames int
		wire           []byte
	}
	run := func(mode string) result {
		s := newScene(t, mode != "off")
		s.show(50, spatial.Point{X: 3, Y: 3})
		for id := int64(100); id < 164; id++ {
			s.show(id, spatial.Point{X: 80 + ((id-100)/32)*80, Y: 80})
		}
		s.show(999, spatial.Point{X: 100, Y: 80})
		switch mode {
		case "overview":
			must(t, s.overview.SetMode(observer, regionsummary.Count))
		case "individual":
			must(t, s.overview.SetMode(observer, regionsummary.Detail))
		}
		s.settle()
		n := s.client.bytes()
		sub := s.manager.AuditStats().Subscriptions
		if mode == "overview" && !maps.Equal(s.client.counts, map[int64]uint64{left: 32, right: 32}) {
			t.Fatal(s.client.counts)
		}
		if _, leak := s.client.held[999]; leak {
			t.Fatal("private object exposed")
		}
		if len(s.client.held) != sub {
			t.Fatal("server/client disagree", s.client.held, sub)
		}
		t.Logf("%s: first screen=%d entitysync bytes in %d frames, %d subscriptions", mode, n, len(s.client.frames), sub)
		return result{n, sub, len(s.client.frames), bytes.Join(s.client.frames, nil)}
	}
	off, unused := run("off"), run("unopened")
	if off.sub != 1 || unused.sub != 1 || unused.frames != off.frames || !bytes.Equal(off.wire, unused.wire) {
		t.Fatal("unopened overlay altered near screen")
	}
	raw, summary := run("individual"), run("overview")
	if raw.sub != 65 || summary.sub != 3 || summary.n >= raw.n {
		t.Fatalf("individual=%+v summary=%+v", raw, summary)
	}
}

func TestFarSwitchKeepsIndependentHoldsAndRefusalKeepsDetail(t *testing.T) {
	s := newScene(t, true, func(c *policy.InterestConfig) { c.MaxPlayersPerView = 1; c.MaxViewsPerPlayer = 3 })
	at := spatial.Point{X: 66, Y: 80}
	s.show(50, at)
	s.show(100, at)
	s.show(101, spatial.Point{X: 160, Y: 80})
	s.show(102, spatial.Point{X: 170, Y: 80})
	must(t, s.interest.Enter(observer, 50, at, policy.ObserverView{})) // independent self, real main and team
	const team = 91
	must(t, s.interest.DefineRelationView(team, config().DetailProfile))
	must(t, s.interest.AddViewMembers(team, 101))
	must(t, s.interest.AttachView(observer, team))
	must(t, s.overview.SetMode(observer, regionsummary.Detail))
	s.settle()
	wantOrdinary := []int64{50, 100, 101}
	ordinary := func() {
		t.Helper()
		if ids := s.overview.OrdinarySeen(observer); !slices.Equal(ids, wantOrdinary) {
			t.Fatalf("ordinary: got %v want %v", ids, wantOrdinary)
		}
	}
	ordinary()
	const other = 12
	must(t, s.interest.Watch(other, spatial.Point{X: 10, Y: 10}, policy.ObserverView{}))
	must(t, s.overview.SetMode(other, regionsummary.Count))
	if err := s.overview.SetMode(observer, regionsummary.Count); !errors.Is(err, policy.ErrViewLimit) {
		t.Fatalf("view full: %v", err)
	}
	must(t, s.interest.Unwatch(other))
	old := s.client.bytes()
	s.settle()
	if s.client.bytes() != old || len(s.client.held) != 4 {
		t.Fatal("refusal changed current detail grant")
	}
	must(t, s.overview.SetMode(observer, regionsummary.Count))
	s.settle()
	ordinary()
	if len(s.client.held) != 5 || s.client.held[102] != "" || s.client.held[50] == "" || s.client.held[100] == "" || s.client.held[101] == "" || !maps.Equal(s.client.counts, map[int64]uint64{left: 1, right: 2}) {
		t.Fatalf("shared sources were revoked or detail-only leaked: %v, %v", s.client.held, s.client.counts)
	}
	t.Logf("same-session detail→count: %d entitysync bytes; independently held self/near/team 3, empty exclusive detail holds", s.client.bytes()-old)
	must(t, s.overview.SetMode(observer, regionsummary.Detail))
	s.settle()
	ordinary()
	if s.client.held[102] == "" || len(s.client.counts) != 0 || len(s.client.held) != 4 {
		t.Fatalf("detail not restored: %v", s.client.held)
	}
	s.overview.Close()
	if s.overview.OrdinarySeen(observer) != nil {
		t.Fatal("closed map retained ordinary query")
	}
	if err := s.overview.SetMode(observer, regionsummary.Detail); err == nil {
		t.Fatal("closed map accepted switch")
	}
}

func TestOrdinarySeenConcurrentSwitchAndClose(t *testing.T) {
	s := newScene(t, true)
	s.show(50, spatial.Point{X: 3, Y: 3})
	s.show(100, spatial.Point{X: 70, Y: 80})
	s.settle()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := range 64 {
			if err := s.overview.SetMode(observer, []regionsummary.Mode{regionsummary.Detail, regionsummary.Count}[i%2]); err != nil {
				return
			}
			s.overview.Place(100, spatial.Point{X: int64(70 + 80*(i%2)), Y: 80})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 64 {
			ids := s.overview.OrdinarySeen(observer)
			if ids != nil && !slices.Equal(ids, []int64{50}) {
				t.Errorf("map-only IDs leaked via ordinary query: %v", ids)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 8 {
			if err := s.manager.Flush(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
		s.overview.Close()
	}()
	close(start)
	wg.Wait()
	s.counts(nil)
	if s.overview.OrdinarySeen(observer) != nil {
		t.Fatal("closed map exposed reused view IDs")
	}
}

func TestInvalidSummaryDoesNotReserveViewsOrSubjects(t *testing.T) {
	s := newScene(t, false)
	tests := []func(*regionsummary.Config){func(c *regionsummary.Config) { c.Public = nil }, func(c *regionsummary.Config) { c.DetailProfile = entity.SyncProfile{} }, func(c *regionsummary.Config) { c.DetailProfile = policy.SelfProfile }, func(c *regionsummary.Config) {
		c.DetailProfile = entity.UnionSyncProfiles(policy.SelfProfile, config().DetailProfile)
	}, func(c *regionsummary.Config) { c.DetailView = c.View }, func(c *regionsummary.Config) { c.Areas[1].Bounds.Min.X-- }, func(c *regionsummary.Config) { c.Areas[0].SubjectID = c.Areas[1].SubjectID }, func(c *regionsummary.Config) { c.Areas[0].Bounds.Min.X = -3 }}
	for _, bad := range tests {
		cfg := config()
		bad(&cfg)
		if m, err := regionsummary.New(s.manager, s.interest, world, cfg); err == nil || m != nil {
			t.Fatal("accepted invalid config", cfg)
		}
	}
	s.show(right, spatial.Point{X: 3, Y: 3})
	if _, err := regionsummary.New(s.manager, s.interest, world, config()); !errors.Is(err, entitysync.ErrSubjectRegistered) {
		t.Fatalf("marker collision: %v", err)
	}
	s.settle()
	if !slices.Equal(s.manager.Subscribers(right), []entitysync.SessionID{observer}) || s.manager.Stats().Subjects != 1 {
		t.Fatal("failed constructor damaged existing subject or leaked a registration")
	}
	if _, ok := s.interest.ViewMembers(config().View); ok {
		t.Fatal("failed constructor left relation behind")
	}
}
