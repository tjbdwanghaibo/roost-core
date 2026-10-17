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
	raws      map[entitysync.SessionID][][]byte // 送达的原始帧，喂给客户端的 frame.Receiver
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
	if lane.raws == nil {
		lane.raws = make(map[entitysync.SessionID][][]byte)
	}
	lane.raws[session] = append(lane.raws[session], append([]byte(nil), raw...))
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

// takeRaw 返回并清空玩家收到的原始帧（送达顺序）。
func (lane *selfLane) takeRaw(player int64) [][]byte {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	raws := lane.raws[entitysync.SessionID(player)]
	delete(lane.raws, entitysync.SessionID(player))
	return raws
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
	clock   *selfClock
}

// selfClock 是 SelfView 读的时钟，只由测试推进：重开丢失会话的时机按它判断。
type selfClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *selfClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *selfClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newSelfFixture(t *testing.T) *selfFixture {
	t.Helper()
	return newSelfFixtureOn(t, &selfLane{})
}

func newSelfFixtureOn(t *testing.T, lane *selfLane) *selfFixture {
	t.Helper()
	fixture := &selfFixture{lane: lane, memory: &memory{}, clock: &selfClock{at: time.Unix(1_000_000, 0)}}
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
	created.now = fixture.clock.now
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

// reopenLost 推进一个同步周期再跑政策阶段：丢失的会话在这次阶段重开。
func (f *selfFixture) reopenLost(t *testing.T) {
	t.Helper()
	f.clock.advance(f.manager.Interval())
	f.flush(t)
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

// Manager 因推送失败丢掉会话（独立 Gate 激活回调返回前连接还不可推送、所有连接都已断开），玩家仍在线时
// 过了重开间隔（半个同步周期）的政策阶段重开会话、重新绑定，以全量恢复；连续失败有上限，新的激活清零。
func TestSelfViewReopensALostSession(t *testing.T) {
	f := newSelfFixture(t)
	f.loaded(t, 7)
	f.lane.setOffline(7, true)
	f.view.Activate(7, "c1")
	f.flush(t) // 第一次推送失败：会话被 Manager 关闭，报告 SessionLost
	if lost := f.manager.Stats().SessionsLost; lost != 1 {
		t.Fatalf("sessions lost = %d, want 1", lost)
	}
	f.lane.setOffline(7, false)
	f.reopenLost(t) // 过了重开间隔的政策阶段重开并重新绑定
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)

	// 一直推不出去：重开次数用尽后不再重开，直到下一次激活。
	f.lane.setOffline(7, true)
	for range 2 * selfMaxReopens {
		f.manager.WakeSync()
		subject, _ := selfSubject(7)
		f.memory.lookup(subject).MarkDirty(1)
		f.reopenLost(t)
	}
	lost := f.manager.Stats().SessionsLost
	if lost > 1+selfMaxReopens+1 {
		t.Fatalf("sessions lost = %d: reopening did not stop after %d attempts", lost, selfMaxReopens)
	}
	f.lane.setOffline(7, false)
	f.flush(t)
	if frames := f.lane.take(7); len(frames) != 0 {
		t.Fatalf("frames after giving up: %+v", frames)
	}
	f.view.Activate(7, "c2")
	f.flush(t)
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)
}

// 连接还在、一次推送被拒让 Manager 丢掉会话后，SelfView 在同一条连接上重开：客户端的接收端已经收过旧
// lifetime 的帧，重开后的全量必须被它接受并从那里继续，不能被当成旧包丢掉——丢掉之后新 lifetime 的
// 增量编号追上旧编号时会被叠到旧基线上，被拒那一帧的变化永远到不了客户端，也没有任何错误。
func TestSelfViewReopenedSessionIsAcceptedByTheSameReceiver(t *testing.T) {
	f := newSelfFixture(t)
	state := f.loaded(t, 7)
	receiver := frame.NewReceiver(frame.DefaultLimits())
	// receive 把玩家收到的原始帧按顺序交给同一个接收端，返回其中被应用的全量个数；丢包或拒收即失败。
	receive := func(stage string) (fulls int) {
		t.Helper()
		for index, raw := range f.lane.takeRaw(7) {
			var kind frame.Kind
			applied, err := receiver.Receive(raw, func(received frame.Frame) error {
				kind = received.Kind
				return nil
			})
			if err != nil {
				t.Fatalf("%s: frame %d refused by the client's receiver: %v", stage, index, err)
			}
			if !applied {
				decoded, _ := frame.Decode(raw, frame.DefaultLimits())
				t.Fatalf("%s: frame %d (%v epoch=%d tick=%d) dropped by the client's receiver as an old packet", stage, index, decoded.Kind, decoded.Epoch, decoded.Tick)
			}
			if kind == frame.Full {
				fulls++
			}
		}
		return fulls
	}

	f.view.Activate(7, "c1")
	f.flush(t)
	for range 3 {
		state.MarkDirty(1)
		f.flush(t)
	}
	if fulls := receive("first lifetime"); fulls != 1 {
		t.Fatalf("first lifetime: %d full snapshots applied, want 1", fulls)
	}

	// 一次推送被拒（如独立 Gate 的可靠队列背压）：连接还在，Manager 丢掉会话并报告 SessionLost。
	f.lane.setOffline(7, true)
	state.MarkDirty(1)
	f.flush(t)
	if lost := f.manager.Stats().SessionsLost; lost != 1 {
		t.Fatalf("sessions lost = %d, want 1", lost)
	}
	f.lane.setOffline(7, false)
	f.reopenLost(t)
	for range 6 {
		state.MarkDirty(1)
		f.flush(t)
	}
	if fulls := receive("reopened lifetime"); fulls != 1 {
		t.Fatalf("reopened lifetime: %d full snapshots applied, want the reopened session's full snapshot", fulls)
	}
}

// on_change 模式下，丢会话会立刻唤醒下一次 Flush（Manager 有释放通知要交付）。SelfView 不能在这些紧跟着的
// 政策阶段里重开：否则“重开 → 推送被拒 → 丢会话 → 唤醒”在毫秒内转满重开次数，传输稍后恢复也收不到任何帧，
// 要等下一次登录。重开要等到丢会话之后至少半个同步周期的政策阶段：周期模式下就是下一个 tick，
// on_change 模式下排除了丢会话引起的立即唤醒，最迟在下一个兜底 tick。
func TestSelfViewDoesNotReopenInTheStagesWokenByTheLoss(t *testing.T) {
	f := newSelfFixture(t)
	state := f.loaded(t, 7)
	f.view.Activate(7, "c1")
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)

	f.lane.setOffline(7, true)
	state.MarkDirty(1)
	f.flush(t)
	if lost := f.manager.Stats().SessionsLost; lost != 1 {
		t.Fatalf("sessions lost = %d, want 1", lost)
	}
	// 时钟不动：这些 Flush 就是丢会话的释放通知唤醒的那几次（OnChangeBatchWindow 默认 0，立即执行）。
	for range 2 * selfMaxReopens {
		f.flush(t)
	}
	if lost := f.manager.Stats().SessionsLost; lost != 1 {
		t.Fatalf("sessions lost = %d after %d stages woken right after the loss: the lost session was reopened in them, "+
			"so a transport that rejects for a moment uses up all %d reopens", lost, 2*selfMaxReopens, selfMaxReopens)
	}

	// 传输恢复，过了半个周期之后的政策阶段重开，玩家重新收到全量。
	f.lane.setOffline(7, false)
	f.clock.advance(f.manager.Interval() / 2)
	f.flush(t)
	f.flush(t)
	requireFull(t, f.lane.take(7), 7)
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
