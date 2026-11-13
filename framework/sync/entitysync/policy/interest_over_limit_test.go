package policy

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ---- 超额降级的政策一侧（D17） ----

// decodingClient 是按会话解帧的接收端：维护每个会话的对象表（ref → 实体 ID），Full 清空旧对象。
type decodingClient struct {
	mu       sync.Mutex
	t        *testing.T
	objects  map[entitysync.SessionID]map[frame.ObjectRef]int64
	creates  map[entitysync.SessionID][]int64 // 每个会话收到过 create 的实体，按到达顺序
	decoders map[entitysync.SessionID]*entitysync.Decoder
}

func newDecodingClient(t *testing.T) *decodingClient {
	return &decodingClient{t: t, objects: make(map[entitysync.SessionID]map[frame.ObjectRef]int64), creates: make(map[entitysync.SessionID][]int64),
		decoders: make(map[entitysync.SessionID]*entitysync.Decoder)}
}

func (c *decodingClient) Push(_ context.Context, session entitysync.SessionID, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	decoder := c.decoders[session]
	if decoder == nil {
		decoder = entitysync.NewDecoder(frame.DefaultLimits())
		c.decoders[session] = decoder
	}
	decoded, err := decoder.Decode(payload)
	if err != nil {
		c.t.Errorf("decode frame: %v", err)
		return nil
	}
	table := c.objects[session]
	if table == nil || decoded.Kind == frame.Full {
		table = make(map[frame.ObjectRef]int64)
		c.objects[session] = table
	}
	for _, object := range decoded.Objects {
		switch object.Operation {
		case frame.ObjectCreate:
			table[object.Ref] = object.SubjectID
			c.creates[session] = append(c.creates[session], object.SubjectID)
		case frame.ObjectRemove:
			if _, held := table[object.Ref]; !held {
				c.t.Errorf("session %d: remove of a ref it does not hold", session)
			}
			delete(table, object.Ref)
		}
	}
	return nil
}

func (c *decodingClient) held(session entitysync.SessionID) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]int64, 0, len(c.objects[session]))
	for _, id := range c.objects[session] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (c *decodingClient) createCount(session entitysync.SessionID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.creates[session])
}

// overLimitScene 是一个会话上限 maxObjects 的 Manager、一个视野不限数量的 Interest（ApplySync，结算在 Flush 的
// 政策阶段）与可控时钟。
type overLimitScene struct {
	manager  *entitysync.Manager
	interest *Interest
	client   *decodingClient
	now      time.Time
	lost     []error
	mode     ApplyMode
}

func newOverLimitScene(t *testing.T, maxObjects, maxVisible int) *overLimitScene {
	return newOverLimitSceneMode(t, maxObjects, maxVisible, ApplySync)
}

func newOverLimitSceneMode(t *testing.T, maxObjects, maxVisible int, mode ApplyMode) *overLimitScene {
	t.Helper()
	scene := &overLimitScene{client: newDecodingClient(t), now: time.Unix(1_000_000, 0), mode: mode}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{
		Transport: scene.client, Limits: frame.Limits{MaxObjects: maxObjects},
		SessionLost: func(_ entitysync.SessionID, cause error) { scene.lost = append(scene.lost, cause) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	interest, err := NewInterest(InterestConfig{
		Manager: manager, ApplyMode: mode,
		AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150, MaxVisible: maxVisible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	interest.now = func() time.Time { return scene.now }
	scene.manager, scene.interest = manager, interest
	return scene
}

func (s *overLimitScene) flush(t *testing.T, times int) {
	t.Helper()
	for range times {
		if s.mode == ApplyAsync {
			// 异步模式的命令在应用线程上执行：先把已登记的事实全部应用（与 flushInterest 相同），再由 Drain 结算。
			// 应用线程在第一条事实到达时就走第一步，可能只取走前半段（用例还在逐个 Show）；若前半段先结算、
			// Manager 按它暂缓，后半段之后才可见，收紧会按通知到达时更大的可见数去减，停在比会话放得下多一个的上限上，
			// 要等 Manager 1 秒后的重新通知才收敛（异步模式的近似语义，见 interest_over_limit.go）。用例要测的是
			// 同一批事实的收敛，不是分批到达，所以结算之前先应用完。
			stepApplier(t, s.interest)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.manager.Drain(ctx)
			cancel()
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
		}
		if err := s.manager.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	if len(s.lost) != 0 {
		t.Fatalf("sessions lost: %v", s.lost)
	}
}

// show 登记目标并放在 watcher 东边 distance 处。
func (s *overLimitScene) show(t *testing.T, id int64, distance int64) {
	t.Helper()
	if s.manager.Subscribers(id) == nil {
		if err := s.manager.Register(subjectState(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.interest.Show(id, spatial.Point{X: 500 + distance, Y: 500}); err != nil {
		t.Fatal(err)
	}
}

func (s *overLimitScene) limitOf(playerID int64) (viewLimit, bool) {
	s.interest.mu.Lock()
	defer s.interest.mu.Unlock()
	main := s.interest.registry.mainOf(playerID)
	if main == nil {
		return viewLimit{}, false
	}
	entry := s.interest.limits[main.handle]
	if entry == nil {
		return viewLimit{}, false
	}
	return *entry, true
}

// 会话满了：Manager 先按实体 ID 留下最远的几个（它不知道距离），通知 Interest；Interest 把主视野收紧到放得下的数量、
// 从最远的开始退订，腾出的位置在下一次 Flush 换成最近的——最终客户端持有自己 + 最近的 N 个，会话从不断开。
// 之后的试探（冷却到期上调一档）若又超额，新进入的目标还没创建就退订（不上线），冷却期加倍；拥挤散去后撤掉收紧。
func TestInterestShrinksAFullSessionToTheNearest(t *testing.T) {
	forEachApplyMode(t, testInterestShrinksAFullSessionToTheNearest)
}

func testInterestShrinksAFullSessionToTheNearest(t *testing.T, mode ApplyMode) {
	limitedBefore := counterValue(limitedViewsMetric) // 进程级加总：前面用例关掉的 Interest 已扣回
	scene := newOverLimitSceneMode(t, 5, 0, mode)
	const watcher = int64(1001)
	player(t, scene.manager, watcher)
	if err := scene.interest.Enter(watcher, watcher, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	// 目标 i 在 10·(9−i) 处：ID 越大越近。Manager 的 ID 排序与距离正好相反。
	for id := int64(1); id <= 8; id++ {
		scene.show(t, id, 10*(9-id))
	}
	scene.flush(t, 4)
	session := entitysync.SessionID(watcher)
	if held := scene.client.held(session); !slices.Equal(held, []int64{5, 6, 7, 8, watcher}) {
		t.Fatalf("client holds %v, want itself and the four nearest [5 6 7 8]", held)
	}
	limit, ok := scene.limitOf(watcher)
	if !ok || limit.limit != 4 || limit.cooldown != overLimitRelaxBase {
		t.Fatalf("main view limit %+v (%v), want 4 with the base cooldown", limit, ok)
	}
	if stats := scene.manager.Stats(); stats.OverLimitWaiting != 0 || stats.SessionsLost != 0 {
		t.Fatalf("stats %+v after converging", stats)
	}
	if got := counterValue(limitedViewsMetric); got != limitedBefore+1 {
		t.Fatalf("%s = %d while the main view is tightened, want %d", limitedViewsMetric, got, limitedBefore+1)
	}
	// 冷却到期：上调一档（不限数量时按当前上限的 1/8，至少 1）→ 目标 4 进入视野 → Manager 暂缓 → 收回，冷却加倍。
	creates := scene.client.createCount(session)
	scene.now = scene.now.Add(overLimitRelaxBase)
	scene.flush(t, 3)
	if held := scene.client.held(session); !slices.Equal(held, []int64{5, 6, 7, 8, watcher}) || scene.client.createCount(session) != creates {
		t.Fatalf("a failed probe reached the client: holds %v, creates %d → %d", held, creates, scene.client.createCount(session))
	}
	if limit, _ := scene.limitOf(watcher); limit.limit != 4 || limit.cooldown != 2*overLimitRelaxBase {
		t.Fatalf("after a failed probe the limit is %+v, want 4 with a doubled cooldown", limit)
	}
	// 冷却期内不再试探。
	scene.now = scene.now.Add(overLimitRelaxBase)
	scene.flush(t, 2)
	if limit, _ := scene.limitOf(watcher); limit.raised || limit.limit != 4 {
		t.Fatalf("probed inside the cooldown: %+v", limit)
	}
	// 两个最近的离开：同一 tick 补入次近的两个（remove 先于 create，不超额）。
	for _, id := range []int64{8, 7} {
		if err := scene.interest.Hide(id); err != nil {
			t.Fatal(err)
		}
	}
	scene.flush(t, 3)
	if held := scene.client.held(session); !slices.Equal(held, []int64{3, 4, 5, 6, watcher}) {
		t.Fatalf("client holds %v, want itself and [3 4 5 6]", held)
	}
	// 拥挤散去：只剩两个目标，上限不再起作用，冷却到期即撤掉。
	for _, id := range []int64{1, 2, 3, 4} {
		if err := scene.interest.Hide(id); err != nil {
			t.Fatal(err)
		}
	}
	scene.flush(t, 1) // 先让离开生效（异步模式在应用线程上），再让冷却到期
	scene.now = scene.now.Add(2 * overLimitRelaxMax)
	scene.flush(t, 3)
	if _, limited := scene.limitOf(watcher); limited {
		t.Fatal("the limit stayed after the crowd left")
	}
	if got := counterValue(limitedViewsMetric); got != limitedBefore {
		t.Fatalf("%s = %d after the limit was lifted, want %d", limitedViewsMetric, got, limitedBefore)
	}
	if held := scene.client.held(session); !slices.Equal(held, []int64{5, 6, watcher}) {
		t.Fatalf("client holds %v, want itself and [5 6]", held)
	}
	// 撤掉之后视野回到不限：再来的目标照常进入（会话没满）。
	scene.show(t, 9, 5)
	scene.flush(t, 2)
	if held := scene.client.held(session); !slices.Equal(held, []int64{5, 6, 9, watcher}) {
		t.Fatalf("client holds %v after the limit was lifted", held)
	}
}

// 视野配置的 MaxVisible 本身大于会话上限（地形视野 80 > 64 这类配置错误）：不断开，稳定在会话上限，去掉的是最远的；
// 放宽到配置值即撤掉收紧。
func TestInterestWithMaxVisibleAboveTheSessionLimitSettlesAtTheLimit(t *testing.T) {
	scene := newOverLimitScene(t, 4, 6)
	const watcher = int64(1001)
	player(t, scene.manager, watcher)
	if err := scene.interest.Enter(watcher, watcher, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 8; id++ {
		scene.show(t, id, 10*(9-id))
	}
	scene.flush(t, 4)
	session := entitysync.SessionID(watcher)
	if held := scene.client.held(session); !slices.Equal(held, []int64{6, 7, 8, watcher}) {
		t.Fatalf("client holds %v, want itself and the three nearest", held)
	}
	if limit, _ := scene.limitOf(watcher); limit.limit != 3 {
		t.Fatalf("limit %+v, want 3", limit)
	}
	// 会话上限够了（这里换成：最近的三个都走了，只剩 5 个候选里最近的三个照样放得下）——目标都离开时撤掉。
	for id := int64(1); id <= 8; id++ {
		if err := scene.interest.Hide(id); err != nil {
			t.Fatal(err)
		}
	}
	scene.now = scene.now.Add(overLimitRelaxBase)
	scene.flush(t, 2)
	if _, limited := scene.limitOf(watcher); limited {
		t.Fatal("limit kept with nothing in view")
	}
}

// 自身（经 self 看自己）永不暂缓：Hold/Ready 后全部对象重新准入时排在最前；自己被 Hide 期间位置被占满，
// 再出现时挤掉一个、同一帧回到客户端。
func TestInterestSelfIsNeverDeferred(t *testing.T) {
	scene := newOverLimitScene(t, 3, 0)
	const watcher = int64(1001)
	player(t, scene.manager, watcher)
	// 目标先进来、ID 都比自己小：按 ID 排序时自己最后，只有钉住才保证它在。
	for id := int64(1); id <= 5; id++ {
		scene.show(t, id, 10*id)
	}
	scene.flush(t, 1)
	if err := scene.interest.Enter(watcher, watcher, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	scene.flush(t, 4)
	session := entitysync.SessionID(watcher)
	if held := scene.client.held(session); !slices.Contains(held, watcher) || len(held) != 3 {
		t.Fatalf("client holds %v, want itself among 3", held)
	}
	if err := scene.manager.HoldSession(session); err != nil {
		t.Fatal(err)
	}
	if err := scene.manager.ReadySession(session); err != nil {
		t.Fatal(err)
	}
	scene.flush(t, 1)
	if held := scene.client.held(session); !slices.Contains(held, watcher) {
		t.Fatalf("after Hold/Ready the first Full lacks the player itself: %v", held)
	}
	if err := scene.interest.Hide(watcher); err != nil {
		t.Fatal(err)
	}
	scene.flush(t, 3)
	if held := scene.client.held(session); slices.Contains(held, watcher) {
		t.Fatalf("hidden self still held: %v", held)
	}
	if err := scene.interest.Show(watcher, spatial.Point{X: 500, Y: 500}); err != nil {
		t.Fatal(err)
	}
	scene.flush(t, 1)
	if held := scene.client.held(session); !slices.Contains(held, watcher) || len(held) > 3 {
		t.Fatalf("self did not come back in the next frame: %v", held)
	}
}

// 没有距离概念的政策（Group）超额时不收紧：订阅暂缓、会话照旧，成员离开腾出位置后恢复。
func TestGroupOverLimitWaitsForRoom(t *testing.T) {
	client := newDecodingClient(t)
	var lost []error
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{
		Transport: client, Limits: frame.Limits{MaxObjects: 2},
		SessionLost: func(_ entitysync.SessionID, cause error) { lost = append(lost, cause) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err := group.Join(1); err != nil {
		t.Fatal(err)
	}
	for id := int64(11); id <= 13; id++ {
		if err := group.AddSubject(subjectState(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	flush := func() {
		t.Helper()
		for range 2 {
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	flush()
	if held := client.held(1); !slices.Equal(held, []int64{11, 12}) || len(lost) != 0 || manager.Stats().OverLimitWaiting != 1 {
		t.Fatalf("client holds %v lost %v waiting %d", held, lost, manager.Stats().OverLimitWaiting)
	}
	if err := group.RemoveSubject(11); err != nil {
		t.Fatal(err)
	}
	flush()
	if held := client.held(1); !slices.Equal(held, []int64{12, 13}) || manager.Stats().OverLimitWaiting != 0 {
		t.Fatalf("after room appeared the client holds %v", held)
	}
}

// 关系视野里的 pair 被暂缓：它没有距离，由主视野让出最远的一个。
func TestInterestRelationPairMakesTheMainViewGiveWay(t *testing.T) {
	scene := newOverLimitScene(t, 4, 0)
	const watcher = int64(1001)
	player(t, scene.manager, watcher)
	if err := scene.interest.Enter(watcher, watcher, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 3; id++ {
		scene.show(t, id, 10*id)
	}
	scene.flush(t, 2)
	// 队友 900 在视野外，经关系视野看见；ID 大，Manager 先暂缓它。
	if err := scene.manager.Register(subjectState(t, 900)); err != nil {
		t.Fatal(err)
	}
	if err := scene.interest.Show(900, spatial.Point{X: 50, Y: 50}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, scene.interest, watcher, 900)
	scene.flush(t, 4)
	session := entitysync.SessionID(watcher)
	if held := scene.client.held(session); !slices.Equal(held, []int64{1, 2, 900, watcher}) {
		t.Fatalf("client holds %v, want the farthest spatial target (3) to give way to the teammate", held)
	}
}
