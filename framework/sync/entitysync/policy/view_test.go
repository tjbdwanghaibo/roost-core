// 视野（D19）：空间 / 关系两类逻辑视野、玩家与视野多对多、按视野计数的合并、同步层级取高或并集、按视野的 MaxVisible、
// 挂接上限、跟随实体、移除视野。断言看 Manager 的订阅表与 packer 实际打包的字段。

package policy

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func mustView(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// 一个玩家多个空间视野：主视野与一个远处的眼位都看见同一个目标，主视野走开不退订（眼位还在看），解除眼位才退订。
func TestOnePlayersSpatialViewsDoNotReleaseEachOther(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	mustView(t, manager.Register(subjectState(t, moverID)))
	mustView(t, interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}))
	mustView(t, interest.Show(moverID, spatial.Point{X: 520, Y: 500}))
	const ward = ViewID(77)
	mustView(t, interest.DefineSpatialView(ward, spatial.Point{X: 540, Y: 500}, ObserverView{}, 0))
	mustView(t, interest.AttachView(watcherID, ward))
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	if got := interest.ViewVisible(ward); !slices.Equal(got, []int64{watcherID, moverID}) {
		t.Fatalf("ward sees %v", got)
	}
	// 主视野（跟随玩家实体）走远：眼位仍持有。
	mustView(t, interest.Move(watcherID, spatial.Point{X: 100, Y: 100}))
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	// 第二个眼位也看见它；解除第一个，仍持有；解除第二个才退订。
	const second = ViewID(78)
	mustView(t, interest.DefineSpatialView(second, spatial.Point{X: 500, Y: 520}, ObserverView{}, 0))
	mustView(t, interest.AttachView(watcherID, second))
	mustApply(t, interest)
	mustView(t, interest.DetachView(watcherID, ward))
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	mustView(t, interest.DetachView(watcherID, second))
	mustApply(t, interest)
	requireSessions(t, manager, moverID)
}

// 共享关系视野（联盟）：一份成员集合挂给全体成员，成员增减只改这一份，扇出到每个挂接的玩家；一个玩家解除只退他自己的。
func TestSharedRelationViewFansMemberChangesOut(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	members := []int64{3001, 3002, 3003}
	for index, id := range members {
		player(t, manager, id)
		mustView(t, interest.Enter(id, id, spatial.Point{X: 100 + int64(index)*400, Y: 100}, ObserverView{}))
	}
	const alliance = ViewID(123)
	mustView(t, interest.DefineRelationView(alliance, entity.SyncProfile{}))
	mustView(t, interest.SetViewMembers(alliance, members[:2]))
	for _, id := range members {
		mustView(t, interest.AttachView(id, alliance))
	}
	mustApply(t, interest)
	requireSessions(t, manager, 3001, 3001, 3002, 3003)
	requireSessions(t, manager, 3002, 3001, 3002, 3003)
	requireSessions(t, manager, 3003, 3003)
	// 3003 入盟：一次 AddViewMembers，全体看见他。
	mustView(t, interest.AddViewMembers(alliance, 3003))
	mustApply(t, interest)
	requireSessions(t, manager, 3003, 3001, 3002, 3003)
	// 3001 退盟：成员去掉、自己解除；剩下的人看不见他，他也看不见别人。
	mustView(t, interest.RemoveViewMembers(alliance, 3001))
	mustView(t, interest.DetachView(3001, alliance))
	mustApply(t, interest)
	requireSessions(t, manager, 3001, 3001)
	requireSessions(t, manager, 3002, 3002, 3003)
	requireSessions(t, manager, 3003, 3002, 3003)
	if players, _ := interest.ViewPlayers(alliance); !slices.Equal(players, []int64{3002, 3003}) {
		t.Fatalf("alliance players = %v", players)
	}
	if got, _ := interest.ViewMembers(alliance); !slices.Equal(got, []int64{3002, 3003}) {
		t.Fatalf("alliance members = %v", got)
	}
}

// 副本阵营视野：阵营里每个单位一个跟随它的空间视野，全部挂给阵营里的每个玩家——任何一个单位看得见，全阵营都看得见；
// 单位走开时只有它那份释放，另一个单位还看着就不退订。
func TestCampViewsShareEveryUnitsSightWithTheWholeCamp(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	camp := []int64{4001, 4002}
	for _, id := range camp {
		mustView(t, manager.OpenSession(entitysync.SessionID(id)))
		mustView(t, interest.Watch(id, spatial.Point{X: 50, Y: 50}, ObserverView{}))
	}
	const unitA, unitB, enemy = int64(5001), int64(5002), int64(6001)
	for _, id := range []int64{unitA, unitB, enemy} {
		mustView(t, manager.Register(subjectState(t, id)))
	}
	mustView(t, interest.Show(unitA, spatial.Point{X: 500, Y: 500}))
	mustView(t, interest.Show(unitB, spatial.Point{X: 900, Y: 900}))
	mustView(t, interest.Show(enemy, spatial.Point{X: 540, Y: 500}))
	for index, unit := range []int64{unitA, unitB} {
		id := ViewID(700 + index)
		mustView(t, interest.DefineSpatialView(id, spatial.Point{}, ObserverView{}, unit))
		for _, member := range camp {
			mustView(t, interest.AttachView(member, id))
		}
	}
	mustApply(t, interest)
	requireSessions(t, manager, enemy, 4001, 4002)
	// B 走到敌人身边，A 走开：A 那份释放，B 那份还在。
	mustView(t, interest.Move(unitB, spatial.Point{X: 560, Y: 500}))
	mustView(t, interest.Move(unitA, spatial.Point{X: 100, Y: 900}))
	mustApply(t, interest)
	requireSessions(t, manager, enemy, 4001, 4002)
	if got := interest.ViewVisible(700); slices.Contains(got, enemy) {
		t.Fatalf("unit A's view still sees the enemy after A walked away: %v", got)
	}
	// B 也走开：全阵营看不见。
	mustView(t, interest.Move(unitB, spatial.Point{X: 900, Y: 100}))
	mustApply(t, interest)
	requireSessions(t, manager, enemy)
}

// ---- 同步层级 ----

// fieldRecorder 是用字段掩码打包的 subject：记下最近一次全量的视图身份与实际打包的字段。
type fieldRecorder struct {
	mu      sync.Mutex
	profile entity.SyncProfile
	fields  uint64
}

func (r *fieldRecorder) last() (entity.SyncProfile, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.profile, r.fields
}

func maskedSubject(t *testing.T, manager *entitysync.Manager, id int64, views *entity.SyncViewSet) *fieldRecorder {
	t.Helper()
	recorder := &fieldRecorder{}
	packer, err := entity.NewMaskedSyncPacker(views, 1, func(fields uint64) ([]byte, error) {
		recorder.mu.Lock()
		recorder.fields = fields
		recorder.mu.Unlock()
		return []byte{byte(fields)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "test", Packer: entity.SubjectSyncPackFunc{
		Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			recorder.mu.Lock()
			recorder.profile = profile
			recorder.mu.Unlock()
			return packer.PackSubjectSnapshot(profile)
		},
		Delta: packer.PackSubjectDelta,
	}})
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	return recorder
}

// 同一目标被几个视野看见：层级是包含关系就只订阅数据多的那个（身份就是它本身），不是包含关系就取并集，
// packer 实际打包的字段是各层级字段的并集。
func TestLayersOfSeveralViewsTakeTheRicherOrTheUnion(t *testing.T) {
	const (
		name, pos, hp, rank uint64 = 1, 2, 4, 8
	)
	near, far, alliance := entity.SyncProfile{Key: "near"}, entity.SyncProfile{Key: "far", LOD: 1}, entity.SyncProfile{Key: "alliance"}
	views, err := entity.NewSyncViewSet(
		entity.SyncView{Profile: entity.SyncProfile{}, Fields: name | pos | hp | rank},
		entity.SyncView{Profile: near, Fields: name | pos | hp},
		entity.SyncView{Profile: far, Fields: name | pos, Priority: 1},
		entity.SyncView{Profile: alliance, Fields: name | rank},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager := newManager(t)
	interest, err := NewInterest(InterestConfig{
		Manager: manager,
		AOI:     AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150, Bands: []int64{40}},
		BandLayer: func(band int) entity.SyncProfile {
			if band == 0 {
				return near
			}
			return far
		},
		ViewSets: map[string]*entity.SyncViewSet{"unit": views},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	player(t, manager, watcherID)
	mustView(t, interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}))
	recorder := maskedSubject(t, manager, moverID, views)
	mustView(t, interest.Show(moverID, spatial.Point{X: 520, Y: 500}))
	expect := func(step string, profile entity.SyncProfile, fields uint64) {
		t.Helper()
		mustApply(t, interest)
		requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
		gotProfile, gotFields := recorder.last()
		if gotProfile != profile.Normalize() || gotFields != fields {
			t.Fatalf("%s: view %+v fields %04b, want %+v fields %04b", step, gotProfile, gotFields, profile.Normalize(), fields)
		}
	}
	expect("near only", near, name|pos|hp)
	// 联盟视野：联盟与近处互不包含，取并集。
	const allianceView = ViewID(900)
	mustView(t, interest.DefineRelationView(allianceView, alliance))
	mustView(t, interest.AddViewMembers(allianceView, moverID))
	mustView(t, interest.AttachView(watcherID, allianceView))
	expect("near + alliance", entity.UnionSyncProfiles(near, alliance), name|pos|hp|rank)
	// 走到远档：远处与联盟仍互不包含。
	mustView(t, interest.Move(moverID, spatial.Point{X: 600, Y: 500}))
	expect("far + alliance", entity.UnionSyncProfiles(far, alliance), name|pos|rank)
	// 再来一个近处的眼位：远处被近处包含，不再单独出现在并集里。
	const ward = ViewID(901)
	mustView(t, interest.DefineSpatialView(ward, spatial.Point{X: 610, Y: 500}, ObserverView{}, 0))
	mustView(t, interest.AttachView(watcherID, ward))
	expect("far + near + alliance", entity.UnionSyncProfiles(near, alliance), name|pos|hp|rank)
	// 退出联盟视野：只剩远处与近处，取包含它的近处，身份就是 near 本身。
	mustView(t, interest.DetachView(watcherID, allianceView))
	expect("far + near", near, name|pos|hp)
}

// MaxVisible 按每个视野计：上限 1 时，两个视野各看见一个目标，玩家看见两个。
func TestMaxVisibleCountsPerView(t *testing.T) {
	manager := newManager(t)
	interest, err := NewInterest(InterestConfig{Manager: manager, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150, MaxVisible: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	mustView(t, manager.OpenSession(entitysync.SessionID(watcherID)))
	mustView(t, interest.Watch(watcherID, spatial.Point{X: 200, Y: 200}, ObserverView{}))
	for _, id := range []int64{11, 12, 21, 22} {
		mustView(t, manager.Register(subjectState(t, id)))
	}
	mustView(t, interest.Show(11, spatial.Point{X: 210, Y: 200}))
	mustView(t, interest.Show(12, spatial.Point{X: 230, Y: 200}))
	mustView(t, interest.Show(21, spatial.Point{X: 810, Y: 800}))
	mustView(t, interest.Show(22, spatial.Point{X: 830, Y: 800}))
	mustView(t, interest.DefineSpatialView(5, spatial.Point{X: 800, Y: 800}, ObserverView{}, 0))
	mustView(t, interest.AttachView(watcherID, 5))
	mustApply(t, interest)
	requireSessions(t, manager, 11, entitysync.SessionID(watcherID))
	requireSessions(t, manager, 21, entitysync.SessionID(watcherID))
	requireSessions(t, manager, 12)
	requireSessions(t, manager, 22)
}

// 挂接上限：一个玩家的视野数（含主视野）与一个视野的玩家数，超出拒绝并计数，拒绝不改变已有挂接。
func TestViewAttachmentLimitsRefuse(t *testing.T) {
	manager := newManager(t)
	interest, err := NewInterest(InterestConfig{Manager: manager, MaxViewsPerPlayer: 2, MaxPlayersPerView: 2, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	player(t, manager, 1)
	mustView(t, interest.Enter(1, 1, spatial.Point{X: 500, Y: 500}, ObserverView{}))
	for _, id := range []ViewID{10, 11} {
		mustView(t, interest.DefineRelationView(id, entity.SyncProfile{}))
	}
	before := counterValue(viewLimitMetric)
	mustView(t, interest.AttachView(1, 10))
	if err := interest.AttachView(1, 11); !errors.Is(err, ErrViewLimit) {
		t.Fatalf("a third view (main + 10 + 11) was attached: %v", err)
	}
	mustView(t, interest.AttachView(1, 10)) // 已挂接的再挂一次不算新挂接
	mustView(t, interest.AttachView(2, 10))
	if err := interest.AttachView(3, 10); !errors.Is(err, ErrViewLimit) {
		t.Fatalf("a third player was attached to view 10: %v", err)
	}
	if got := counterValue(viewLimitMetric) - before; got != 2 {
		t.Fatalf("limit refusals counted %d, want 2", got)
	}
	if players, _ := interest.ViewPlayers(10); !slices.Equal(players, []int64{1, 2}) {
		t.Fatalf("refusal changed the attachments: %v", players)
	}
	// Watch / Enter 建主视野也计入：已挂两个视野的玩家进不来。
	mustView(t, interest.AttachView(3, 11))
	mustView(t, interest.DefineRelationView(12, entity.SyncProfile{}))
	mustView(t, interest.AttachView(3, 12))
	mustView(t, manager.OpenSession(3))
	if err := interest.Watch(3, spatial.Point{X: 100, Y: 100}, ObserverView{}); !errors.Is(err, ErrViewLimit) {
		t.Fatalf("a main view past the limit was created: %v", err)
	}
}

// 空间视野总数（维护者 2026-10-22）：每个 Interest 经 DefineSpatialView 定义的空间视野不超过 MaxSpatialViews，挂没挂接
// 都算（定义了没挂接的也是 AOI 观察者，MaxViewsPerPlayer 管不到）；超出返回 ErrSpatialViewLimit 并计数，什么都不定义。
// 主视野与关系视野不计；RemoveView 释放名额；重复 ID 仍报 ErrViewExists。零值取 DefaultMaxSpatialViews，负数拒绝构造。
func TestSpatialViewsPerInterestAreLimited(t *testing.T) {
	manager := newManager(t)
	config := InterestConfig{Manager: manager, MaxSpatialViews: 2, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
	}}
	interest, err := NewInterest(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	player(t, manager, 1)
	mustView(t, interest.Enter(1, 1, spatial.Point{X: 500, Y: 500}, ObserverView{}))
	mustView(t, interest.DefineRelationView(5, entity.SyncProfile{}))
	at := spatial.Point{X: 100, Y: 100}
	mustView(t, interest.DefineSpatialView(10, at, ObserverView{}, 0))
	mustView(t, interest.DefineSpatialView(11, at, ObserverView{}, 0)) // 没挂接给任何人，照样占名额
	before := counterValueWith(viewLimitMetric, "reason", "spatial_views")
	if err := interest.DefineSpatialView(12, at, ObserverView{}, 0); !errors.Is(err, ErrSpatialViewLimit) {
		t.Fatalf("a third spatial view was defined past MaxSpatialViews 2: %v", err)
	}
	if got := counterValueWith(viewLimitMetric, "reason", "spatial_views") - before; got != 1 {
		t.Fatalf("spatial view refusals counted %d, want 1", got)
	}
	if _, ok := interest.ViewPlayers(12); ok {
		t.Fatal("the refused spatial view was defined")
	}
	if err := interest.DefineSpatialView(10, at, ObserverView{}, 0); !errors.Is(err, ErrViewExists) {
		t.Fatalf("a duplicate id at the limit: %v, want ErrViewExists", err)
	}
	mustView(t, interest.DefineRelationView(6, entity.SyncProfile{})) // 关系视野不计
	mustView(t, interest.RemoveView(10))
	mustView(t, interest.DefineSpatialView(12, at, ObserverView{}, 0))

	config.MaxSpatialViews = 0
	if in, err := NewInterest(config); err != nil || in.registry.maxSpatial != DefaultMaxSpatialViews {
		t.Fatalf("zero MaxSpatialViews: err %v", err)
	} else {
		in.Close()
	}
	config.MaxSpatialViews = -1
	if _, err := NewInterest(config); !errors.Is(err, ErrInterestConfig) {
		t.Fatalf("negative MaxSpatialViews: %v", err)
	}
}

// 跟随实体可改、可为空：视野随跟随的实体移动并排除它；改成不跟随后停在原地，改成跟随另一个实体就跳到那里。
func TestSpatialViewFollowCanChangeOrBeCleared(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	mustView(t, manager.OpenSession(entitysync.SessionID(watcherID)))
	mustView(t, interest.Watch(watcherID, spatial.Point{X: 50, Y: 950}, ObserverView{}))
	const unit, other, target = int64(81), int64(82), int64(83)
	for _, id := range []int64{unit, other, target} {
		mustView(t, manager.Register(subjectState(t, id)))
	}
	mustView(t, interest.Show(unit, spatial.Point{X: 100, Y: 100}))
	mustView(t, interest.Show(other, spatial.Point{X: 900, Y: 100}))
	mustView(t, interest.Show(target, spatial.Point{X: 500, Y: 500}))
	const eye = ViewID(9)
	mustView(t, interest.DefineSpatialView(eye, spatial.Point{X: 950, Y: 950}, ObserverView{}, unit))
	mustView(t, interest.AttachView(watcherID, eye))
	mustApply(t, interest)
	// 定义时的 at 被跟随实体的位置取代；跟随的实体自己不进可见集。
	requireSessions(t, manager, target)
	requireSessions(t, manager, unit)
	mustView(t, interest.Move(unit, spatial.Point{X: 520, Y: 500}))
	mustApply(t, interest)
	requireSessions(t, manager, target, entitysync.SessionID(watcherID))
	requireSessions(t, manager, unit)
	// 不再跟随：单位走开，视野留下。
	mustView(t, interest.SetViewFollow(eye, 0))
	mustView(t, interest.Move(unit, spatial.Point{X: 100, Y: 100}))
	mustApply(t, interest)
	requireSessions(t, manager, target, entitysync.SessionID(watcherID))
	// 改成跟随另一个实体：视野跳到它那里，之前跟随的单位如果进入范围也照常可见。
	mustView(t, interest.SetViewFollow(eye, other))
	mustApply(t, interest)
	requireSessions(t, manager, target)
	if got := interest.ViewVisible(eye); slices.Contains(got, other) {
		t.Fatalf("a view sees the entity it follows: %v", got)
	}
	mustView(t, interest.Move(other, spatial.Point{X: 110, Y: 100}))
	mustApply(t, interest)
	requireSessions(t, manager, unit, entitysync.SessionID(watcherID))
}

// 移除视野自动从所有挂接的玩家解除；同一个 ViewID 之后可以重新定义成另一类视野。
func TestRemoveViewDetachesEveryPlayer(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	for _, id := range []int64{1, 2} {
		mustView(t, manager.OpenSession(entitysync.SessionID(id)))
		mustView(t, interest.Watch(id, spatial.Point{X: 50, Y: 50}, ObserverView{}))
	}
	const near, far = int64(31), int64(32)
	mustView(t, manager.Register(subjectState(t, near)))
	mustView(t, manager.Register(subjectState(t, far)))
	mustView(t, interest.Show(near, spatial.Point{X: 500, Y: 500}))
	mustView(t, interest.Show(far, spatial.Point{X: 900, Y: 900}))
	const spatialView, relationView = ViewID(41), ViewID(42)
	mustView(t, interest.DefineSpatialView(spatialView, spatial.Point{X: 510, Y: 500}, ObserverView{}, 0))
	mustView(t, interest.DefineRelationView(relationView, entity.SyncProfile{}))
	mustView(t, interest.SetViewMembers(relationView, []int64{far}))
	for _, id := range []int64{1, 2} {
		mustView(t, interest.AttachView(id, spatialView))
		mustView(t, interest.AttachView(id, relationView))
	}
	mustApply(t, interest)
	requireSessions(t, manager, near, 1, 2)
	requireSessions(t, manager, far, 1, 2)
	mustView(t, interest.RemoveView(spatialView))
	mustView(t, interest.RemoveView(relationView))
	mustApply(t, interest)
	requireSessions(t, manager, near)
	requireSessions(t, manager, far)
	if err := interest.AttachView(1, spatialView); !errors.Is(err, ErrViewUnknown) {
		t.Fatalf("attached a removed view: %v", err)
	}
	// 重新定义（换成关系视野）后照常工作。
	mustView(t, interest.DefineRelationView(spatialView, entity.SyncProfile{}))
	mustView(t, interest.SetViewMembers(spatialView, []int64{near}))
	mustView(t, interest.AttachView(2, spatialView))
	mustApply(t, interest)
	requireSessions(t, manager, near, 2)
}

// 视野 ID 0 留给主视野；种类不对的操作与未知视野都拒绝；Leave 解除玩家的全部挂接，视野本身保留给其他玩家。
func TestViewIDsKindsAndLeave(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	if err := interest.DefineRelationView(MainView, entity.SyncProfile{}); !errors.Is(err, ErrInterestConfig) {
		t.Fatalf("view id 0 accepted: %v", err)
	}
	mustView(t, interest.DefineRelationView(1, entity.SyncProfile{}))
	if err := interest.DefineSpatialView(1, spatial.Point{X: 1, Y: 1}, ObserverView{}, 0); !errors.Is(err, ErrViewExists) {
		t.Fatalf("redefined a view: %v", err)
	}
	if err := interest.MoveSpatialView(1, spatial.Point{X: 2, Y: 2}); !errors.Is(err, ErrViewKind) {
		t.Fatalf("moved a relation view: %v", err)
	}
	if err := interest.SetViewMembers(2, nil); !errors.Is(err, ErrViewUnknown) {
		t.Fatalf("unknown view: %v", err)
	}
	if err := interest.DefineSpatialView(3, spatial.Point{X: -1, Y: 0}, ObserverView{}, 0); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("out of bounds view: %v", err)
	}
	if err := interest.DefineSpatialView(3, spatial.Point{X: 1, Y: 1}, ObserverView{LOD: 3}, 0); !errors.Is(err, ErrObserverView) {
		t.Fatalf("unknown LOD level: %v", err)
	}
	player(t, manager, 7)
	player(t, manager, 8)
	mustView(t, manager.Register(subjectState(t, 99)))
	mustView(t, interest.Enter(7, 7, spatial.Point{X: 100, Y: 100}, ObserverView{}))
	mustView(t, interest.Enter(8, 8, spatial.Point{X: 900, Y: 900}, ObserverView{}))
	mustView(t, interest.Show(99, spatial.Point{X: 500, Y: 500}))
	mustView(t, interest.SetViewMembers(1, []int64{99, 8}))
	mustView(t, interest.AttachView(7, 1))
	mustView(t, interest.AttachView(8, 1))
	mustApply(t, interest)
	requireSessions(t, manager, 99, 7, 8)
	mustView(t, interest.Leave(7, 7))
	mustApply(t, interest)
	requireSessions(t, manager, 99, 8)
	if players, _ := interest.ViewPlayers(1); !slices.Equal(players, []int64{8}) {
		t.Fatalf("players after Leave = %v", players)
	}
	// 8 离场：他的实体从成员里去掉，视野还在（没有挂接的玩家）。
	mustView(t, interest.Leave(8, 8))
	mustApply(t, interest)
	if members, ok := interest.ViewMembers(1); !ok || !slices.Equal(members, []int64{99}) {
		t.Fatalf("members after Leave = %v, %v", members, ok)
	}
}

// 启动期块预算按 worstObserverBlocks × MaxViewsPerPlayer 核算：调大一个玩家的视野数，超过 MaxPlayerBlocks 拒绝构造。
func TestPlayerBlockBudgetCountsEveryView(t *testing.T) {
	manager := newManager(t)
	config := InterestConfig{Manager: manager, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
	}}
	worst := config.AOI.withViewDefaults().worstObserverBlocks()
	config.MaxPlayerBlocks = int(worst) * 4
	config.MaxViewsPerPlayer = 4
	in, err := NewInterest(config)
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	config.MaxViewsPerPlayer = 5
	if _, err := NewInterest(config); !errors.Is(err, ErrInterestBudget) {
		t.Fatalf("5 views × %d blocks against a budget of %d: %v", worst, config.MaxPlayerBlocks, err)
	}
}
