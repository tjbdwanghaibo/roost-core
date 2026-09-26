package Game

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"example.com/planet/game/chatroom"
	player "example.com/planet/game/entities/player"
	world "example.com/planet/game/entities/world"
	"example.com/planet/game/handler"
	syncsender "example.com/planet/game/handler/syncsender"
	gamescene "example.com/planet/game/scene"
	accessplayertcp "example.com/planet/internal/access/player/tcp"
	"example.com/planet/protocol/msgid"
	"example.com/planet/protocol/pb"
	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// What this asserts is the whole claim of sync=true: a change made on the
// server, through the ordinary DAO setters, reaches ANOTHER player's client
// as a decodable delta — without anyone writing a push for it.
//
// The only stand-in is the socket. The subject, the room, the wire encoding
// and the client-side decode are the real framework, which is the point: a
// test that packed its own payload would prove nothing about the pipeline
// that carries it.

// sceneRecorder stands in for the player TCP Runtime, keeping what each
// player would have received.
type sceneRecorder struct {
	mu          sync.Mutex
	pushes      map[int64][]*pb.EntitySyncPush
	offline     map[int64]bool
	unreachable map[int64]error // a push to this player fails with this error
	unavailable bool            // the whole access layer is gone: every push fails
	closed      []func(accessplayertcp.SessionClosed)
}

func newSceneRecorder() *sceneRecorder {
	return &sceneRecorder{pushes: make(map[int64][]*pb.EntitySyncPush)}
}

func (recorder *sceneRecorder) PushPlayer(_ context.Context, playerID int64, messageID uint32, value any) error {
	if messageID != msgid.MsgEntitySync {
		return nil
	}
	push, ok := value.(*pb.EntitySyncPush)
	if !ok {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.unavailable {
		return accessplayertcp.ErrTransportUnavailable
	}
	if err := recorder.unreachable[playerID]; err != nil {
		return err
	}
	recorder.pushes[playerID] = append(recorder.pushes[playerID], push)
	return nil
}

// disconnect makes one player unreachable the way the real access layer
// reports it: no session, so every push to them fails. Nobody is told —
// this is the case the session lifecycle source has NOT fired for yet.
func (recorder *sceneRecorder) disconnect(playerID int64) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.unreachable == nil {
		recorder.unreachable = make(map[int64]error)
	}
	recorder.unreachable[playerID] = fmt.Errorf("%w: player %d", accessplayertcp.ErrSessionNotFound, playerID)
	if recorder.offline == nil {
		recorder.offline = make(map[int64]bool)
	}
	recorder.offline[playerID] = true
}

func (recorder *sceneRecorder) setUnavailable(unavailable bool) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.unavailable = unavailable
}

// Every recorded player counts as connected unless a test says otherwise.
func (recorder *sceneRecorder) ActiveSessions(playerID int64) int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.offline[playerID] {
		return 0
	}
	return 1
}

// OnSessionClosed is the access layer's session lifecycle source, stood in
// for: the test closes a session by calling the subscriber directly.
func (recorder *sceneRecorder) OnSessionClosed(fn func(accessplayertcp.SessionClosed)) func() {
	recorder.mu.Lock()
	recorder.closed = append(recorder.closed, fn)
	recorder.mu.Unlock()
	return func() {}
}

func (recorder *sceneRecorder) close(playerID int64) {
	recorder.mu.Lock()
	if recorder.offline == nil {
		recorder.offline = make(map[int64]bool)
	}
	recorder.offline[playerID] = true
	subscribers := append([]func(accessplayertcp.SessionClosed){}, recorder.closed...)
	recorder.mu.Unlock()
	for _, fn := range subscribers {
		fn(accessplayertcp.SessionClosed{PlayerID: playerID, SessionID: "gone"})
	}
}

func (recorder *sceneRecorder) take(playerID int64) []*pb.EntitySyncPush {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	taken := recorder.pushes[playerID]
	delete(recorder.pushes, playerID)
	return taken
}

// decodeSceneFrames turns what one player received into the subject updates
// it carries: each push is one frame, each object one subject.
func decodeSceneFrames(t *testing.T, _ int64, pushes []*pb.EntitySyncPush) map[int64]bson.M {
	t.Helper()
	out := make(map[int64]bson.M)
	for _, push := range pushes {
		decoded, err := entitysync.DecodeFrame(push.Payload, frame.DefaultLimits())
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		for _, object := range decoded.Objects {
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					t.Fatalf("decode subject update: %v", err)
				}
				payload := update.Payload.BytesCopy()
				if len(payload) == 0 {
					continue
				}
				var document bson.M
				if err := bson.Unmarshal(payload, &document); err != nil {
					t.Fatalf("decode player sync payload: %v", err)
				}
				out[update.SubjectID] = document
			}
		}
	}
	return out
}

// removesIn counts the ObjectRemoves a player received.
func removesIn(t *testing.T, pushes []*pb.EntitySyncPush) int {
	t.Helper()
	removes := 0
	for _, push := range pushes {
		decoded, err := entitysync.DecodeFrame(push.Payload, frame.DefaultLimits())
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		for _, object := range decoded.Objects {
			if object.Operation == frame.ObjectRemove {
				removes++
			}
		}
	}
	return removes
}

// joinReady is what the login path does end to end: Join (session held) and
// the client's scene_ready (session released).
func joinReady(t *testing.T, scene *Scene, ctx context.Context, subject *player.Player, at spatial.Point) {
	t.Helper()
	if err := scene.Join(ctx, subject, at); err != nil {
		t.Fatalf("join %d: %v", subject.ID(), err)
	}
	if err := scene.Ready(entity.GetUniqueIDFromEntityID(subject.ID())); err != nil {
		t.Fatalf("ready %d: %v", subject.ID(), err)
	}
}

func newScenePlayer(t *testing.T, uniqueID int64) *player.Player {
	t.Helper()
	player.RegisterEntity()
	id, err := entity.BuildEntityID(uniqueID, player.EntityKindPlayer)
	if err != nil {
		t.Fatal(err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: player.EntityKindPlayer, Id: id})
	if err != nil {
		t.Fatalf("build player %d: %v", uniqueID, err)
	}
	value, ok := built.(*player.Player)
	if !ok {
		t.Fatalf("the generated factory produced %T", built)
	}
	return value
}

func newSceneWorld(t *testing.T) *world.World {
	t.Helper()
	world.RegisterEntity()
	id, err := entity.BuildEntityID(1, world.EntityKindWorld)
	if err != nil {
		t.Fatal(err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: world.EntityKindWorld, Id: id})
	if err != nil {
		t.Fatalf("build world: %v", err)
	}
	return built.(*world.World)
}

// sceneCommitter keeps what a transaction would have written instead of
// writing it: this test is about what goes out on the wire, not what lands in
// Mongo.
type sceneCommitter struct{}

func (sceneCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

func newSceneNest(t *testing.T, entities map[int64]entity.IThreadSafeEntity) nest.Client {
	t.Helper()
	handler.RegisterAddExpNestHandlers()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(sceneGetter(entities)),
		nest.NestOptionWithTransactionCommitter(sceneCommitter{}),
		nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16),
	)
	if err := engine.Start(); err != nil {
		t.Fatalf("start nest: %v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	return engine
}

type sceneGetter map[int64]entity.IThreadSafeEntity

func (g sceneGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	value, ok := g[id]
	if !ok {
		return nil, fmt.Errorf("the harness holds no entity %d", id)
	}
	return value, nil
}

func (g sceneGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		value, err := g.Get(ctx, id, categories[i])
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}

func TestSceneReplicatesOnePlayersChangeToAnother(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	const (
		watcherID = int64(9001)
		moverID   = int64(9002)
	)
	watcher := newScenePlayer(t, watcherID)
	mover := newScenePlayer(t, moverID)
	// Close enough to see each other: the enter radius is 120.
	joinReady(t, scene, ctx, watcher, spatial.Point{X: 500, Y: 500})
	joinReady(t, scene, ctx, mover, spatial.Point{X: 520, Y: 500})
	if scene.Members() != 2 {
		t.Fatalf("the scene holds %d members, want 2", scene.Members())
	}
	// Joining owes snapshots on the next tick; this test is about what a
	// CHANGE produces, so run that tick and start from a clean slate.
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after joins: %v", err)
	}
	recorder.take(watcherID)

	// An ordinary server-side mutation, through the real AddExp transaction.
	// It has to be a transaction: the generated persist setters refuse to run
	// outside one, which is also why replication cannot be a side effect of
	// "someone called a setter" — it is a side effect of a committed change.
	// Nothing in the handler knows about replication.
	counters := newSceneWorld(t)
	client := newSceneNest(t, map[int64]entity.IThreadSafeEntity{
		watcher.ID(): watcher, mover.ID(): mover, counters.ID(): counters,
	})
	gained, err := syncsender.NewAddExpSender(client).MultiSync_AddExp(ctx, mover.ID(), counters.ID(), 250)
	if err != nil {
		t.Fatalf("add exp: %v", err)
	}
	if gained == 0 {
		t.Fatal("the mutation did not level the player up, so the delta would be exp only")
	}
	if !mover.Sync().PendingDirty() {
		t.Fatal("the subject was not marked dirty; PublishSyncDirty did not reach it")
	}

	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	updates := decodeSceneFrames(t, watcherID, recorder.take(watcherID))
	document, ok := updates[mover.ID()]
	if !ok {
		t.Fatalf("the watcher received no update for subject %d; got %v", mover.ID(), updates)
	}
	// The delta carries the fields the mask named — the ones AddExp changed.
	if _, ok := document["level"]; !ok {
		t.Errorf("the delta does not carry the level that changed: %v", document)
	}
	if _, ok := document["exp"]; !ok {
		t.Errorf("the delta does not carry the exp that changed: %v", document)
	}
	// And not the ones it did not: a delta is not a snapshot.
	if _, ok := document["items"]; ok {
		t.Errorf("the delta carries a field the mutation never touched: %v", document)
	}
}

// A player who leaves stops being replicated, and the others are told the
// object is gone rather than left with a stale copy of it.
func TestSceneRetiresASubjectOnLeave(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})
	watcher := newScenePlayer(t, 9101)
	leaver := newScenePlayer(t, 9102)
	joinReady(t, scene, ctx, watcher, spatial.Point{X: 500, Y: 500})
	joinReady(t, scene, ctx, leaver, spatial.Point{X: 510, Y: 500})
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after joins: %v", err)
	}
	recorder.take(9101)

	scene.Leave(ctx, leaver.ID())
	if scene.Members() != 1 {
		t.Fatalf("the scene holds %d members after a leave, want 1", scene.Members())
	}
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after leave: %v", err)
	}
	if removesIn(t, recorder.take(9101)) != 1 {
		t.Error("the watcher was not told the leaver's object is gone")
	}
}

// A player who disconnects while nothing else is happening must leave the
// scene anyway. Before the access layer published session closes, the only
// thing that noticed was a failed push — which never happens if nobody is
// pushing, so an idle world kept its ghosts (RR-20260918-06).
func TestAClosedSessionLeavesTheSceneWithoutAnyTraffic(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	scene.watchSessions(recorder)
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	const goneID = int64(9301)
	subject := newScenePlayer(t, goneID)
	joinReady(t, scene, ctx, subject, spatial.Point{X: 500, Y: 500})
	if scene.Members() != 1 {
		t.Fatalf("the scene holds %d members after a join, want 1", scene.Members())
	}

	recorder.close(goneID)

	// The removal runs on its own goroutine so the lifecycle source is never
	// held by a subscriber.
	deadline := time.Now().Add(2 * time.Second)
	for scene.Members() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if members := scene.Members(); members != 0 {
		t.Fatalf("the scene still holds %d members two seconds after the session closed, with no traffic to notice it", members)
	}
}

// U-0278（W-2026-09-22-03）：一个推不到的玩家不能让同一批里排在它后面的人少收帧。
//
// 房间把一次 flush 的帧按观察者 id 升序排成一批交给这条 lane。lane 以前逐个推、第一个
// 失败就整批返回错误——排在前面的已经推出去了（下一 tick 再收一遍），排在后面的一个
// 都没推。RR-20260922-01 的实跑里，16 个客户端有 13 个从第一个断线者起再没收到任何帧。
//
// 承诺：推不到的那个玩家自己离开场景；其余人这一帧照常到达；批次不报错。
func TestAnUnreachablePlayerDoesNotStarveTheOthers(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	// Three observers of one change, sorted by id: the one in the middle is
	// the one whose connection has gone.
	const (
		firstID = int64(9401)
		goneID  = int64(9402)
		lastID  = int64(9403)
	)
	first := newScenePlayer(t, firstID)
	gone := newScenePlayer(t, goneID)
	last := newScenePlayer(t, lastID)
	for i, member := range []*player.Player{first, gone, last} {
		joinReady(t, scene, ctx, member, spatial.Point{X: 500 + int64(i)*10, Y: 500})
	}
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after joins: %v", err)
	}
	recorder.take(firstID)
	recorder.take(goneID)
	recorder.take(lastID)
	recorder.disconnect(goneID)

	counters := newSceneWorld(t)
	client := newSceneNest(t, map[int64]entity.IThreadSafeEntity{
		first.ID(): first, gone.ID(): gone, last.ID(): last, counters.ID(): counters,
	})
	if _, err := syncsender.NewAddExpSender(client).MultiSync_AddExp(ctx, first.ID(), counters.ID(), 250); err != nil {
		t.Fatalf("add exp: %v", err)
	}
	flushErr := scene.Flush(ctx)

	updates := decodeSceneFrames(t, lastID, recorder.take(lastID))
	if _, ok := updates[first.ID()]; !ok {
		t.Fatalf("the observer sorted after the unreachable one received nothing (flush error: %v); got %v", flushErr, updates)
	}
	if flushErr != nil {
		t.Fatalf("one unreachable observer failed the whole batch: %v", flushErr)
	}
	// The unreachable player's session was closed by the manager, which
	// tells the scene; the removal runs on its own goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for scene.Members() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if members := scene.Members(); members != 2 {
		t.Fatalf("the scene holds %d members two seconds after a push to one of them failed, want 2", members)
	}
}

// The other kind of push failure: the access layer itself is gone (starting,
// stopping). That is nobody's fault in particular, so the batch must fail so
// the room keeps the subject dirty and retries — and no player may be thrown
// out of the scene for it.
func TestAnUnavailableTransportKeepsTheBatchAndThePlayers(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	const (
		moverID   = int64(9501)
		watcherID = int64(9502)
	)
	mover := newScenePlayer(t, moverID)
	watcher := newScenePlayer(t, watcherID)
	joinReady(t, scene, ctx, mover, spatial.Point{X: 500, Y: 500})
	joinReady(t, scene, ctx, watcher, spatial.Point{X: 510, Y: 500})
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after joins: %v", err)
	}
	recorder.setUnavailable(true)

	counters := newSceneWorld(t)
	client := newSceneNest(t, map[int64]entity.IThreadSafeEntity{
		mover.ID(): mover, watcher.ID(): watcher, counters.ID(): counters,
	})
	if _, err := syncsender.NewAddExpSender(client).MultiSync_AddExp(ctx, mover.ID(), counters.ID(), 250); err != nil {
		t.Fatalf("add exp: %v", err)
	}
	if err := scene.Flush(ctx); !errors.Is(err, accessplayertcp.ErrTransportUnavailable) {
		t.Fatalf("flush with the transport gone: %v, want ErrTransportUnavailable so the room retries", err)
	}
	if !mover.Sync().PendingDirty() {
		t.Fatal("the change was dropped: the subject is no longer dirty although nobody received it")
	}
	// Give an (incorrect) asynchronous removal time to happen before asserting
	// that it did not.
	time.Sleep(100 * time.Millisecond)
	if members := scene.Members(); members != 2 {
		t.Fatalf("a transport outage threw players out of the scene: members=%d, want 2", members)
	}
}

// The id-space contract, pinned rather than commented.
//
// The interest system and the manager's subjects carry the **full entity
// id** — the id that is unique across kinds, as opposed to a unique id, which
// is only unique within one (a Player 42 and a Monster 42 share it). The
// manager's sessions are whatever the transport addresses; in this demo that
// is the player id, and sessionFor is the one place that crosses from one
// space to the other.
func TestTheBridgeKeepsEveryIdInTheEntityIdSpace(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	const uniqueID = int64(9401)
	subject := newScenePlayer(t, uniqueID)
	joinReady(t, scene, ctx, subject, spatial.Point{X: 500, Y: 500})

	// The subject the manager holds is the entity id, not the unique id.
	if subject.ID() == uniqueID {
		t.Fatal("this player's entity id equals its unique id, so this test cannot tell the two spaces apart")
	}
	subscribers := scene.manager.Subscribers(subject.ID())
	if len(subscribers) == 0 {
		t.Fatal("the player is not subscribed to its own subject")
	}
	// And the one place that leaves the entity id space is sessionFor, on
	// its way to a transport session: the session IS the player id.
	for _, session := range subscribers {
		if session != entitysync.SessionID(uniqueID) {
			t.Errorf("a session id is %d; this bridge's sessions are player ids (%d)", session, uniqueID)
		}
	}
	if got := entity.GetUniqueIDFromEntityID(subject.ID()); got != uniqueID {
		t.Fatalf("the translation gives %d, want the player id %d", got, uniqueID)
	}
}

// The other half of the same source: chat presence.
//
// The first round of RR-20260918-06 wired the scene and left presence to the
// lazy path — the chat push removes a player when a push to them fails and
// they have no sessions left. In a quiet world nothing pushes, so an offline
// player stayed in the world channel's recipient set indefinitely. The 09-19
// review called that a partial fix, correctly.
func TestAClosedSessionLeavesTheWorldChannel(t *testing.T) {
	recorder := &sceneRecorder{}
	presence := chatroom.DefaultPresence()
	const playerID = int64(4242)
	presence.Add(playerID)
	t.Cleanup(func() { presence.Remove(playerID) })

	unsubscribe := subscribePresence(recorder, recorder)
	defer unsubscribe()

	recorder.close(playerID)
	if presenceHolds(presence, playerID) {
		t.Fatal("the player is still in the world channel's recipient set after their last session closed")
	}

	// A player with another connection still up stays: one closed session is
	// not one offline player.
	const stillOnline = int64(4243)
	presence.Add(stillOnline)
	t.Cleanup(func() { presence.Remove(stillOnline) })
	recorder.mu.Lock()
	subscribers := append([]func(accessplayertcp.SessionClosed){}, recorder.closed...)
	recorder.mu.Unlock()
	for _, fn := range subscribers {
		fn(accessplayertcp.SessionClosed{PlayerID: stillOnline, SessionID: "one-of-two"})
	}
	if !presenceHolds(presence, stillOnline) {
		t.Fatal("a player with another live connection was removed from the world channel")
	}
}

func presenceHolds(presence *chatroom.Presence, playerID int64) bool {
	for _, id := range presence.Snapshot() {
		if id == playerID {
			return true
		}
	}
	return false
}

// U-0267 · C5 · RR-20260920-06：房间拒绝的 subscribe 必须重发。
//
// The matchmaker forms a team before every member has reached the scene. The
// team source claims the pair at once, so the subscribe for the member who
// has not joined yet is sent for a subject the room does not hold, and fails.
// The interest system marked that pair subscribed BEFORE the room was called
// and only re-emits on a band change — so once the member does join, nothing
// mentions the pair again and the teammate stays invisible for the rest of
// the session.
func TestSceneRetriesASubscribeTheRoomRefused(t *testing.T) {
	recorder := newSceneRecorder()
	scene, err := newScene(recorder, nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	ctx := context.Background()
	if err := scene.Start(ctx); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scene.Close(closeCtx)
	})

	const (
		watcherID = int64(9101)
		laterID   = int64(9102)
	)
	watcher := newScenePlayer(t, watcherID)
	later := newScenePlayer(t, laterID)

	joinReady(t, scene, ctx, watcher, spatial.Point{X: 100, Y: 100})

	// The team forms while the second member is still loading. The two are
	// far enough apart that distance never claims the pair: being teammates
	// is the only reason these two should see each other.
	scene.SetTeam([]int64{watcherID, laterID})
	recorder.take(watcherID)

	// The teammate arrives. This is the tick that has to make good on the
	// subscribe the room refused a moment ago.
	joinReady(t, scene, ctx, later, spatial.Point{X: 900, Y: 900})

	subscribed := false
	for _, session := range scene.manager.Subscribers(later.ID()) {
		if session == entitysync.SessionID(watcherID) {
			subscribed = true
		}
	}
	if !subscribed {
		t.Fatalf("the watcher is not subscribed to teammate %d after the teammate joined: %v",
			later.ID(), scene.manager.Subscribers(later.ID()))
	}
	if err := scene.Flush(ctx); err != nil {
		t.Fatalf("flush after the teammate joined: %v", err)
	}

	// And what arrives is the teammate's whole state, not the tail of a
	// stream the watcher missed the beginning of: a retried subscribe takes
	// the same path as a first one, so it captures a snapshot.
	updates := decodeSceneFrames(t, watcherID, recorder.take(watcherID))
	document, ok := updates[later.ID()]
	if !ok {
		t.Fatalf("the watcher received nothing for teammate %d; got %v", later.ID(), updates)
	}
	for _, field := range []string{"_id", "level", "exp"} {
		if _, ok := document[field]; !ok {
			t.Errorf("the retried subscribe delivered %q-less state, so it was not a snapshot: %v", field, document)
		}
	}
}
