// 自身视图（SelfView）：玩家会话的激活 / 关闭、实体在内存里出现 / 卸载 / 撤回、Manager 丢掉会话，
// 各种先后顺序下玩家都只看到自己，并且新连接的第一帧是全量。

package policy

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// selfLane 是测试用的玩家推送：按会话（玩家）记录解码后的帧；玩家没有连接时像 PushSyncPlayer 一样失败；
// block 非 nil 时第一次推送等它关闭，用来让关闭与 Flush 的推送阶段并发。
type selfLane struct {
	mu        sync.Mutex
	frames    map[entitysync.SessionID][]selfFrame
	offline   map[entitysync.SessionID]bool
	block     chan struct{}
	entered   chan struct{}
	blockOnce sync.Once
}

type selfFrame struct {
	kind    frame.Kind
	objects []selfObject
}

type selfObject struct {
	operation frame.ObjectOperation
	subject   int64 // remove 没有 subject 内容，记 0
	full      bool
	profile   string
}

var errNoConnection = errors.New("self lane: player has no connection")

func (lane *selfLane) Push(_ context.Context, session entitysync.SessionID, raw []byte) error {
	if lane.block != nil {
		lane.blockOnce.Do(func() {
			close(lane.entered)
			<-lane.block
		})
	}
	decoded, err := frame.Decode(raw, frame.DefaultLimits())
	if err != nil {
		return err
	}
	record := selfFrame{kind: decoded.Kind}
	for _, object := range decoded.Objects {
		entry := selfObject{operation: object.Operation}
		for _, component := range object.Components {
			update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
			if err != nil {
				return err
			}
			entry.subject, entry.full, entry.profile = update.SubjectID, update.Full, update.Profile.Key
		}
		record.objects = append(record.objects, entry)
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.offline[session] {
		return fmt.Errorf("%w: %d", errNoConnection, session)
	}
	if lane.frames == nil {
		lane.frames = make(map[entitysync.SessionID][]selfFrame)
	}
	lane.frames[session] = append(lane.frames[session], record)
	return nil
}

func (lane *selfLane) setOffline(player int64, offline bool) {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.offline == nil {
		lane.offline = make(map[entitysync.SessionID]bool)
	}
	lane.offline[entitysync.SessionID(player)] = offline
}

// take 返回并清空玩家收到的帧。
func (lane *selfLane) take(player int64) []selfFrame {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	frames := lane.frames[entitysync.SessionID(player)]
	delete(lane.frames, entitysync.SessionID(player))
	return frames
}

// memory 是测试里的“内存中已发布的实体”：Lookup 只读它，不加载。
type memory struct {
	mu     sync.Mutex
	states map[int64]*entity.SubjectSyncState
}

func (m *memory) put(state *entity.SubjectSyncState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states == nil {
		m.states = make(map[int64]*entity.SubjectSyncState)
	}
	m.states[state.SubjectID()] = state
}

func (m *memory) drop(subject int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, subject)
}

func (m *memory) lookup(subject int64) *entity.SubjectSyncState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[subject]
}

// selfSubject 把玩家 ID 映射成一个不同的 subject ID，测试据此确认 SelfView 没有把两种 ID 混用。
func selfSubject(player int64) (int64, error) { return player + 1_000_000, nil }

type selfFixture struct {
	manager *entitysync.Manager
	lane    *selfLane
	memory  *memory
	view    *SelfView
}

func newSelfFixture(t *testing.T) *selfFixture {
	t.Helper()
	return newSelfFixtureOn(t, &selfLane{})
}

func newSelfFixtureOn(t *testing.T, lane *selfLane) *selfFixture {
	t.Helper()
	fixture := &selfFixture{lane: lane, memory: &memory{}}
	var view atomic.Pointer[SelfView]
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{
		Transport: lane,
		// Manager 在 SelfView 之前构造，SessionLost 经转发接上（正式接线 wiring/selfsync 同样如此）。
		SessionLost: func(session entitysync.SessionID, cause error) {
			if current := view.Load(); current != nil {
				current.SessionLost(session, cause)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.manager = manager
	created, err := NewSelfView(SelfViewConfig{Manager: manager, Subject: selfSubject, Lookup: fixture.memory.lookup})
	if err != nil {
		t.Fatal(err)
	}
	view.Store(created)
	fixture.view = created
	t.Cleanup(func() {
		_ = manager.Close(context.Background())
		created.Close()
	})
	return fixture
}

// loaded 让玩家实体出现在内存里（像 DataEngine 加载或 GetOrCreate 新建之后那样），返回它的同步状态。
func (f *selfFixture) loaded(t *testing.T, player int64) *entity.SubjectSyncState {
	t.Helper()
	subject, _ := selfSubject(player)
	state := subjectState(t, subject)
	f.memory.put(state)
	return state
}

func (f *selfFixture) flush(t *testing.T) {
	t.Helper()
	if err := f.manager.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// requireFull 断言玩家收到的帧里第一帧是全量、只含它自己（profile self），之后没有别的 subject。
func requireFull(t *testing.T, frames []selfFrame, player int64) {
	t.Helper()
	subject, _ := selfSubject(player)
	if len(frames) == 0 {
		t.Fatalf("player %d received nothing, want a full snapshot of itself", player)
	}
	first := frames[0]
	if first.kind != frame.Full || len(first.objects) != 1 || first.objects[0].subject != subject || !first.objects[0].full || first.objects[0].profile != SelfProfile.Key {
		t.Fatalf("player %d first frame = %+v, want a Full with one full %q update of subject %d", player, first, SelfProfile.Key, subject)
	}
	for _, later := range frames[1:] {
		for _, object := range later.objects {
			if object.operation != frame.ObjectRemove && object.subject != subject {
				t.Fatalf("player %d received subject %d: a self view only carries the player itself", player, object.subject)
			}
		}
	}
}

func TestSelfViewSendsAFullSnapshotThenDeltas(t *testing.T) {
	f := newSelfFixture(t)
	state := f.loaded(t, 7)
	f.view.Activate(7, "c1")
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)

	state.MarkDirty(1)
	f.flush(t)
	frames := f.lane.take(7)
	if len(frames) != 1 || frames[0].kind != frame.Delta || len(frames[0].objects) != 1 || frames[0].objects[0].full {
		t.Fatalf("after a change: %+v, want one Delta carrying the change", frames)
	}
}

// 登录时实体常常还不在内存里（首登新建、第一次请求才冷加载）：会话照开，实体一出现就送全量，
// 不论它是加载出来的（EntityLoaded）还是新建的（没有通知，靠政策阶段复查）。
func TestSelfViewWaitsForTheEntityToAppear(t *testing.T) {
	for _, notified := range []bool{true, false} {
		t.Run(fmt.Sprintf("notified=%v", notified), func(t *testing.T) {
			f := newSelfFixture(t)
			f.view.Activate(7, "c1")
			f.flush(t)
			if frames := f.lane.take(7); len(frames) != 0 {
				t.Fatalf("frames before the entity exists: %+v", frames)
			}
			f.loaded(t, 7)
			if notified {
				subject, _ := selfSubject(7)
				f.view.EntityLoaded(subject)
			}
			f.flush(t)
			requireFull(t, f.lane.take(7), 7)
		})
	}
}

// 断线重连：新连接的第一帧必须是全量。旧连接关闭与新连接激活之间可能有也可能没有政策阶段。
func TestSelfViewReconnectGetsAFullSnapshot(t *testing.T) {
	for _, stageBetween := range []bool{true, false} {
		t.Run(fmt.Sprintf("stage_between=%v", stageBetween), func(t *testing.T) {
			f := newSelfFixture(t)
			state := f.loaded(t, 7)
			f.view.Activate(7, "c1")
			f.flush(t)
			state.MarkDirty(1)
			f.flush(t)
			f.lane.take(7)

			f.view.Deactivate(7, "c1")
			if stageBetween {
				f.flush(t)
			}
			f.view.Activate(7, "c2")
			f.flush(t)
			requireFull(t, f.lane.take(7), 7)
			if stats := f.manager.Stats(); stats.Sessions != 1 || stats.Subjects != 1 || stats.Subscriptions != 1 {
				t.Fatalf("after reconnect: sessions=%d subjects=%d subscriptions=%d, want 1/1/1", stats.Sessions, stats.Subjects, stats.Subscriptions)
			}
		})
	}
}

// 顶号：同一玩家的新连接先激活、旧连接后关闭（或反过来）。玩家始终在线，视图不能被旧连接的关闭带走，
// 新连接拿到全量。
func TestSelfViewReplacedConnectionKeepsTheView(t *testing.T) {
	for _, order := range []string{"new-then-close-old", "close-old-then-new"} {
		for _, stageBetween := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stage_between=%v", order, stageBetween), func(t *testing.T) {
				f := newSelfFixture(t)
				state := f.loaded(t, 7)
				f.view.Activate(7, "old")
				f.flush(t)
				f.lane.take(7)
				first, second := func() { f.view.Activate(7, "new") }, func() { f.view.Deactivate(7, "old") }
				if order == "close-old-then-new" {
					first, second = second, first
				}
				first()
				if stageBetween {
					f.flush(t)
				}
				second()
				f.flush(t)
				frames := f.lane.take(7)
				if order == "new-then-close-old" || !stageBetween {
					requireFull(t, frames, 7)
				} else {
					// 旧连接关闭在先、并且中间过了一个政策阶段：视图先拆、新连接到时重开，仍以全量开始。
					requireFull(t, frames, 7)
				}
				if f.view.Online() != 1 {
					t.Fatalf("online players = %d, want 1: the player still has a connection", f.view.Online())
				}
				state.MarkDirty(1)
				f.flush(t)
				if frames := f.lane.take(7); len(frames) != 1 || frames[0].kind != frame.Delta {
					t.Fatalf("change after the replacement: %+v, want one Delta on the live view", frames)
				}
			})
		}
	}
}

// Manager 因推送被拒丢掉会话之后，SelfView 不在原连接上重开：传输拒收时会关闭这名玩家的连接
// （wiring/selfsync.PushPlayerFrame），这里等关闭通知（Deactivate）收尾。不论之后跑多少个政策阶段、传输是否
// 恢复，原连接都不再收到帧；连接关闭后 subject 注销，客户端重连（新激活）从新 epoch 的全量开始。
func TestSelfViewWaitsForTheConnectionToCloseAfterALoss(t *testing.T) {
	f := newSelfFixture(t)
	state := f.loaded(t, 7)
	f.view.Activate(7, "c1")
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)

	f.lane.setOffline(7, true)
	state.MarkDirty(1)
	f.flush(t) // 推送被拒：会话被 Manager 丢掉，报告 SessionLost
	f.lane.setOffline(7, false)
	for range 20 {
		state.MarkDirty(1)
		f.flush(t)
	}
	if frames := f.lane.take(7); len(frames) != 0 {
		t.Fatalf("frames after the loss on the connection that refused: %+v; the session must not be reopened on it", frames)
	}
	if stats := f.manager.Stats(); stats.Sessions != 0 || stats.SessionsLost != 1 {
		t.Fatalf("after the loss: sessions=%d lost=%d, want 0/1", stats.Sessions, stats.SessionsLost)
	}

	f.view.Deactivate(7, "c1") // 传输关闭了这条连接
	f.flush(t)
	if stats := f.manager.Stats(); stats.Sessions != 0 || stats.Subjects != 0 || f.view.Online() != 0 {
		t.Fatalf("after the close: sessions=%d subjects=%d online=%d, want all 0", stats.Sessions, stats.Subjects, f.view.Online())
	}
	f.view.Activate(7, "c2") // 客户端重连
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)
}

// 连着多次偶发的拒收，每次都由“连接被关闭 → 客户端重连”恢复：每一次都是新激活，没有跨激活累积的次数，
// 不会在某一次之后永久失去视图。lane 在拒收时像 PushPlayerFrame 一样关闭玩家的连接（关闭通知 Deactivate）。
func TestSelfViewEveryIsolatedLossIsRecoveredByTheReconnect(t *testing.T) {
	f := newSelfFixture(t)
	state := f.loaded(t, 7)
	f.view.Activate(7, "c0")
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)
	for round := 1; round <= 20; round++ {
		connection := fmt.Sprintf("c%d", round-1)
		f.lane.setOffline(7, true)
		state.MarkDirty(1)
		f.flush(t)
		f.view.Deactivate(7, connection) // 拒收的连接被关闭
		f.lane.setOffline(7, false)
		f.view.Activate(7, fmt.Sprintf("c%d", round))
		f.flush(t)
		frames := f.lane.take(7)
		if len(frames) == 0 {
			t.Fatalf("isolated loss #%d: the reconnected client received nothing; its view is gone", round)
		}
		requireFull(t, frames, 7)
	}
	if lost := f.manager.Stats().SessionsLost; lost != 20 {
		t.Fatalf("sessions lost = %d, want 20 (one per refused push)", lost)
	}
}

// 丢会话与新连接激活竞态（顶号、快速重连）：新连接在丢会话的通知处理之前或之后激活，旧连接的关闭在其前或其后
// 到达，新连接都按正常激活拿到新会话的全量，之后照常收增量；旧连接的关闭不带走它。
func TestSelfViewLossRacingANewConnection(t *testing.T) {
	for _, order := range []string{"activate-then-close-old", "close-old-then-activate"} {
		for _, stageBetween := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stage_between=%v", order, stageBetween), func(t *testing.T) {
				f := newSelfFixture(t)
				state := f.loaded(t, 7)
				f.view.Activate(7, "old")
				f.flush(t)
				requireFull(t, f.lane.take(7), 7)

				f.lane.setOffline(7, true)
				state.MarkDirty(1)
				f.flush(t) // 推送被拒：SessionLost 已记下，下个政策阶段处理
				f.lane.setOffline(7, false)
				first, second := func() { f.view.Activate(7, "new") }, func() { f.view.Deactivate(7, "old") }
				if order == "close-old-then-activate" {
					first, second = second, first
				}
				first()
				if stageBetween {
					f.flush(t)
				}
				second()
				f.flush(t)
				requireFull(t, f.lane.take(7), 7)
				state.MarkDirty(1)
				f.flush(t)
				if frames := f.lane.take(7); len(frames) != 1 || frames[0].kind != frame.Delta {
					t.Fatalf("change on the new connection: %+v, want one Delta", frames)
				}
				if stats := f.manager.Stats(); stats.Sessions != 1 || f.view.Online() != 1 {
					t.Fatalf("sessions=%d online=%d, want 1/1", stats.Sessions, f.view.Online())
				}
			})
		}
	}
}

// Stranded 数“在线却超过 selfStrandedPeriods 个同步周期没有打开的会话”的玩家：传输违反约定、丢会话后没有
// 关闭连接时它上升，连接关闭（或新激活打开了会话）后归零。登录后等实体出现（会话已开）不算。
func TestSelfViewCountsPlayersOnlineWithoutASession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSelfFixture(t)
		state := f.loaded(t, 7)
		f.view.Activate(7, "c1")
		f.view.Activate(8, "c1") // 实体不在内存：会话开着、等实体出现，不算滞留
		f.flush(t)
		f.lane.take(7)
		f.lane.setOffline(7, true)
		state.MarkDirty(1)
		f.flush(t) // 玩家 7 的会话丢了，而连接一直没有关闭
		f.flush(t)
		if got := f.view.Stranded(); got != 0 {
			t.Fatalf("stranded right after the loss = %d, want 0 (the close notification may still be on its way)", got)
		}
		time.Sleep(time.Duration(selfStrandedPeriods+1) * f.manager.Interval())
		f.flush(t)
		if got := f.view.Stranded(); got != 1 {
			t.Fatalf("stranded %d periods after the loss = %d, want 1", selfStrandedPeriods+1, got)
		}
		f.view.Deactivate(7, "c1")
		f.flush(t)
		if got := f.view.Stranded(); got != 0 {
			t.Fatalf("stranded after the connection closed = %d, want 0", got)
		}

		// 新激活打开了会话也归零。
		f.lane.setOffline(7, false)
		f.view.Activate(7, "c2")
		f.flush(t)
		f.lane.setOffline(7, true)
		state.MarkDirty(1)
		f.flush(t) // 丢会话
		f.flush(t) // 下一个政策阶段处理它：从这里开始算没有会话的时长
		time.Sleep(time.Duration(selfStrandedPeriods+1) * f.manager.Interval())
		f.flush(t)
		if got := f.view.Stranded(); got != 1 {
			t.Fatalf("stranded after the second loss = %d, want 1", got)
		}
		f.lane.setOffline(7, false)
		f.view.Activate(7, "c3")
		f.flush(t)
		if got := f.view.Stranded(); got != 0 {
			t.Fatalf("stranded after a new connection opened a session = %d, want 0", got)
		}
	})
}

// 卸载后重载不了退回 remove（RetractUnloadedSubject），实体之后又被加载：Direct 的重新提交恢复绑定，
// 玩家重新收到全量。
func TestSelfViewUnloadRetractionIsResubmitted(t *testing.T) {
	f := newSelfFixture(t)
	old := f.loaded(t, 7)
	f.view.Activate(7, "c1")
	f.flush(t)
	f.lane.take(7)
	subject, _ := selfSubject(7)
	old.Close()
	f.memory.drop(subject)
	if !f.manager.RetractUnloadedSubject(subject) {
		t.Fatal("retract refused")
	}
	f.flush(t)
	frames := f.lane.take(7)
	if len(frames) != 1 || len(frames[0].objects) != 1 || frames[0].objects[0].operation != frame.ObjectRemove {
		t.Fatalf("after the retraction: %+v, want one remove", frames)
	}
	f.loaded(t, 7)
	f.view.EntityLoaded(subject)
	f.flush(t)
	f.flush(t) // 重新提交在下一次政策阶段交付
	frames = f.lane.take(7)
	if len(frames) == 0 || frames[0].objects[0].operation != frame.ObjectCreate || !frames[0].objects[0].full {
		t.Fatalf("after the reload: %+v, want the player again as a full create", frames)
	}
}

// 事务内新建的玩家被回滚（Nest 调 RetractSyncSubject）：Direct 释放了绑定，下一次新建出现时重新绑定。
func TestSelfViewRolledBackCreateIsBoundAgain(t *testing.T) {
	f := newSelfFixture(t)
	f.view.Activate(7, "c1")
	created := f.loaded(t, 7)
	f.flush(t)
	f.lane.take(7)
	subject, _ := selfSubject(7)
	f.manager.RetractSyncSubject(created)
	f.memory.drop(subject)
	f.flush(t)
	f.flush(t)
	f.lane.take(7)
	f.loaded(t, 7)
	f.flush(t)
	f.flush(t)
	frames := f.lane.take(7)
	if len(frames) == 0 || frames[0].objects[0].operation != frame.ObjectCreate || frames[0].objects[0].subject != subject {
		t.Fatalf("after the second create: %+v, want the player as a create", frames)
	}
	if stats := f.manager.Stats(); stats.Subjects != 1 || stats.Subscriptions != 1 {
		t.Fatalf("subjects=%d subscriptions=%d, want 1/1", stats.Subjects, stats.Subscriptions)
	}
}

// 会话关闭与 Flush 并发：关闭发生在 Flush 推送阶段之中，下一个政策阶段拆掉会话与 subject；
// 推送失败报告的 SessionLost 不能把已经离线的玩家重开。
func TestSelfViewCloseDuringFlush(t *testing.T) {
	lane := &selfLane{block: make(chan struct{}), entered: make(chan struct{})}
	f := newSelfFixtureOn(t, lane)
	f.loaded(t, 7)
	f.view.Activate(7, "c1")
	done := make(chan error, 1)
	go func() { done <- f.manager.Flush(context.Background()) }()
	<-lane.entered
	f.view.Deactivate(7, "c1")
	lane.setOffline(7, true) // 连接已经不在：这次推送失败
	close(lane.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.flush(t)
	f.flush(t)
	if stats := f.manager.Stats(); stats.Sessions != 0 || stats.Subjects != 0 || stats.Subscriptions != 0 || f.view.Online() != 0 {
		t.Fatalf("after close during flush: sessions=%d subjects=%d subscriptions=%d online=%d, want all 0", stats.Sessions, stats.Subjects, stats.Subscriptions, f.view.Online())
	}
}

// 登录与断线并发：多个玩家、每人多条连接，激活 / 关闭与 Flush 在不同 goroutine 交错。结束后在线的玩家
// 恰好各有一个会话、一条自身订阅；全部离线的玩家不留会话与 subject。
func TestSelfViewLoginAndDisconnectRace(t *testing.T) {
	f := newSelfFixture(t)
	const players, connections = 16, 4
	for player := int64(1); player <= players; player++ {
		f.loaded(t, player)
	}
	stop := make(chan struct{})
	var flushes sync.WaitGroup
	flushes.Add(1)
	go func() {
		defer flushes.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = f.manager.Flush(context.Background())
			}
		}
	}()
	var churn sync.WaitGroup
	keep := map[int64]bool{}
	for player := int64(1); player <= players; player++ {
		keep[player] = player%2 == 0
		// 每个玩家几条连接各自一个 goroutine 反复登录 / 断线；要留在线的玩家另有一条连接在中途登录、不再断开，
		// 其他连接的断线不能带走它的视图。
		for connection := range connections {
			churn.Add(1)
			go func(player int64, connection string) {
				defer churn.Done()
				random := rand.New(rand.NewPCG(uint64(player), uint64(len(connection))))
				for round := range 50 {
					f.view.Activate(player, connection)
					if round%3 == 0 {
						f.memory.lookup(player + 1_000_000).MarkDirty(1)
					}
					if random.IntN(2) == 0 {
						_ = f.manager.Flush(context.Background())
					}
					f.view.Deactivate(player, connection)
				}
			}(player, fmt.Sprintf("c%d", connection))
		}
		if keep[player] {
			churn.Add(1)
			go func(player int64) {
				defer churn.Done()
				f.view.Activate(player, "stay")
			}(player)
		}
	}
	churn.Wait()
	close(stop)
	flushes.Wait()
	f.flush(t)
	f.flush(t)
	stats := f.manager.Stats()
	want := players / 2
	if stats.Sessions != want || stats.Subjects != want || stats.Subscriptions != want || f.view.Online() != want {
		t.Fatalf("sessions=%d subjects=%d subscriptions=%d online=%d, want %d each", stats.Sessions, stats.Subjects, stats.Subscriptions, f.view.Online(), want)
	}
	for player := int64(1); player <= players; player++ {
		if !keep[player] {
			continue
		}
		subject, _ := selfSubject(player)
		subscribers := f.manager.Subscribers(subject)
		if len(subscribers) != 1 || subscribers[0] != entitysync.SessionID(player) {
			t.Fatalf("player %d subscribers = %v, want only itself", player, subscribers)
		}
	}
}

func TestNewSelfViewRefusesAnIncompleteConfig(t *testing.T) {
	manager := newManager(t)
	for name, config := range map[string]SelfViewConfig{
		"manager": {Subject: selfSubject, Lookup: (&memory{}).lookup},
		"subject": {Manager: manager, Lookup: (&memory{}).lookup},
		"lookup":  {Manager: manager, Subject: selfSubject},
	} {
		if _, err := NewSelfView(config); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}
