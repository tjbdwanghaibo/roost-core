package physicsroom

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestRoomInputProtobuf(t *testing.T) {
	payload := protowire.AppendTag(nil, 1, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 300)
	payload = protowire.AppendTag(payload, 2, protowire.Fixed32Type)
	payload = protowire.AppendFixed32(payload, math.Float32bits(-10))
	payload = protowire.AppendTag(payload, 4, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	payload = protowire.AppendTag(payload, 5, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 3)
	got, err := decodeInput(payload)
	if err != nil || got.Sequence != 300 || got.Acceleration[0] != -10 || !got.Jump || got.GrabTarget != 3 {
		t.Fatalf("input=%+v err=%v", got, err)
	}
	for _, bad := range [][]byte{nil, {8}, {8, 0}, {8, 1, 8, 2}, {8, 1, 32, 2}, {8, 1, 21, 0}, {8, 1, 18, 0}, {8, 1, 40, 1, 40, 2}, {8, 1, 45, 0, 0, 0, 0}, {8, 1, 48, 0}} {
		if _, err := decodeInput(bad); err == nil {
			t.Fatalf("invalid protobuf accepted: %x", bad)
		}
	}
}

// fakeRooms 记录调用；Poll 返回空帧，pollErr 非空时让 Relay 以错误结束。
type fakeRooms struct {
	domain.Rooms
	joins, submits, leaves atomic.Int64
	pollErr                atomic.Pointer[error]
}

func (f *fakeRooms) Join(_ context.Context, room domain.Ref, binding gateway.Binding, _ uint64) (domain.Member, error) {
	f.joins.Add(1)
	return domain.Member{Room: room, PlayerID: binding.PlayerID, Token: "token", InputGeneration: 9}, nil
}
func (f *fakeRooms) Submit(context.Context, domain.Member, domain.Input) error {
	f.submits.Add(1)
	return nil
}
func (f *fakeRooms) Poll(_ context.Context, m domain.Member, ack uint64) (domain.Delivery, error) {
	if err := f.pollErr.Load(); err != nil {
		return domain.Delivery{}, *err
	}
	return domain.Delivery{Room: m.Room, Sequence: ack}, nil
}
func (f *fakeRooms) Leave(context.Context, domain.Member) error { f.leaves.Add(1); return nil }

type fakeGame struct{ binding gateway.Binding }

func (fakeGame) PushBound(context.Context, gateway.Binding, uint32, wire.PayloadKind, []byte) error {
	return nil
}
func (g fakeGame) SessionReceiver(id string) (gateway.Binding, uint64, bool) {
	return g.binding, 1, id == g.binding.SessionID
}

type fakeSession struct {
	id     string
	closed chan error
}

func (s fakeSession) Principal() gateway.Principal {
	return gateway.Principal{PlayerID: 1, ServerID: 2, SessionID: s.id}
}
func (fakeSession) Reply(context.Context, any) error { return nil }
func (s fakeSession) Close(err error) error {
	select {
	case s.closed <- err:
	default:
	}
	return nil
}

func ingressBinding() gateway.Binding {
	return gateway.Binding{PlayerID: 1, SessionID: "session", ConnectionNonce: "0123456789abcdef0123456789abcdef", BindID: "1123456789abcdef0123456789abcdef", Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "0123456789abcdef"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "1123456789abcdef"}}
}

func inputPayload(seq uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), seq)
}

// 连接生命周期：首个输入 Join 一次并启动唯一 Relay；断开取消 Relay 并 Leave；Relay 出错关闭连接；Close 排空并拒绝后续。
func TestPlayerIngressJoinsOnceAndReleasesWithTheConnection(t *testing.T) {
	ctx := context.Background()
	if _, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 1, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) { return domain.Ref{}, 0, nil }}); err == nil {
		t.Fatal("same input and frame message id accepted")
	}
	var seats atomic.Int64
	room := domain.Ref{ID: "room", Epoch: 1}
	p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
		seats.Add(1)
		return room, 51, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding := ingressBinding()
	session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
	if _, err = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, domain.ErrStopping) || p.Ready() {
		t.Fatalf("dispatch before attach: %v ready=%v", err, p.Ready())
	}
	rooms := &fakeRooms{}
	if err = p.Attach(rooms, fakeGame{binding: binding}); err != nil || !p.Ready() {
		t.Fatalf("attach: %v ready=%v", err, p.Ready())
	}
	if _, err = p.Dispatch(ctx, session, 3, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("foreign message id: %v", err)
	}
	if _, err = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1)); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("input before OnActive: %v", err)
	}
	if err = p.OnActive(binding, 1); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		reply, err := p.Dispatch(ctx, session, 2, uint32(seq), wire.PayloadProtobuf, inputPayload(seq))
		if err != nil {
			t.Fatal(err)
		}
		r, ok := reply.(*gateway.TCPResponse)
		if !ok || r.MessageID != 2 || r.Sequence != uint32(seq) {
			t.Fatalf("reply=%+v", reply)
		}
		// RoomInputAck：input_generation=1 是成员代际。
		if _, _, n := protowire.ConsumeTag(r.Payload); n < 0 {
			t.Fatalf("input ack=%x", r.Payload)
		} else if generation, m := protowire.ConsumeVarint(r.Payload[n:]); m < 0 || generation != 9 {
			t.Fatalf("input ack generation=%d payload=%x", generation, r.Payload)
		}
	}
	if seats.Load() != 1 || rooms.joins.Load() != 1 || rooms.submits.Load() != 3 {
		t.Fatalf("seats=%d joins=%d submits=%d", seats.Load(), rooms.joins.Load(), rooms.submits.Load())
	}
	p.OnClosed(binding, 1)
	waitFor(t, func() bool { return rooms.leaves.Load() == 1 })
	if _, err = p.Dispatch(ctx, session, 2, 4, wire.PayloadProtobuf, inputPayload(4)); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("input after close: %v", err)
	}

	// 新连接重新 Join；Relay 因 RPC 错误结束时关闭该连接。
	if err = p.OnActive(binding, 2); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("poll failed")
	rooms.pollErr.Store(&failure)
	if _, err = p.Dispatch(ctx, session, 2, 5, wire.PayloadProtobuf, inputPayload(5)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-session.closed:
		if !errors.Is(got, failure) {
			t.Fatalf("session closed with %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("relay failure did not close the session")
	}
	if rooms.joins.Load() != 2 {
		t.Fatalf("joins=%d", rooms.joins.Load())
	}

	stop, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = p.Close(stop); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("ready after close")
	}
	if err = p.OnActive(binding, 3); !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("OnActive after close: %v", err)
	}
	if _, err = p.Dispatch(ctx, session, 2, 6, wire.PayloadProtobuf, inputPayload(6)); !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("dispatch after close: %v", err)
	}
}

// 内容握手：消息号与回调必须成对配置；空载荷请求按已认证绑定返回 RoomLevel，不 Join、不建房。
func TestPlayerIngressContentHandshake(t *testing.T) {
	ctx := context.Background()
	seat := func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
		return domain.Ref{ID: "room", Epoch: 1}, 51, nil
	}
	content := func(_ context.Context, b gateway.Binding) (RoomContent, error) {
		return RoomContent{Data: []byte(`{"Version":1}`), SHA256: "abc", Body: uint64(50 + b.PlayerID)}, nil
	}
	for _, bad := range []PlayerIngressConfig{
		{InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 3},
		{InputMessageID: 2, FrameMessageID: 1, Seat: seat, Content: content},
		{InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 2, Content: content},
		{InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 1, Content: content},
	} {
		if _, err := NewPlayerIngress(bad); err == nil {
			t.Fatalf("invalid content config accepted: %+v", bad)
		}
	}
	p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 3, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	binding := ingressBinding()
	rooms := &fakeRooms{}
	if err = p.Attach(rooms, fakeGame{binding: binding}); err != nil {
		t.Fatal(err)
	}
	session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
	if _, err = p.Dispatch(ctx, session, 3, 1, wire.PayloadProtobuf, []byte{8, 1}); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("non-empty content request: %v", err)
	}
	reply, err := p.Dispatch(ctx, session, 3, 2, wire.PayloadProtobuf, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := reply.(*gateway.TCPResponse)
	data := r.Payload
	_, _, n := protowire.ConsumeTag(data)
	level, m := protowire.ConsumeBytes(data[n:])
	data = data[n+m:]
	_, _, n = protowire.ConsumeTag(data)
	hash, m := protowire.ConsumeString(data[n:])
	data = data[n+m:]
	_, _, n = protowire.ConsumeTag(data)
	body, m := protowire.ConsumeVarint(data[n:])
	if r.MessageID != 3 || r.Sequence != 2 || string(level) != `{"Version":1}` || hash != "abc" || body != 51 || m < 0 || len(data[n+m:]) != 0 {
		t.Fatalf("content reply=%+v", r)
	}
	if rooms.joins.Load() != 0 {
		t.Fatal("content handshake joined a room")
	}
}

// 关闭与输入并发：Close 之后不会再启动新的 Relay（WaitGroup 不在 Wait 之后 Add），在途 Join 立即 Leave。
func TestPlayerIngressCloseRacesInput(t *testing.T) {
	ctx := context.Background()
	for range 20 {
		rooms := &fakeRooms{}
		binding := ingressBinding()
		p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
			return domain.Ref{ID: "room", Epoch: 1}, 51, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Attach(rooms, fakeGame{binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err = p.OnActive(binding, 1); err != nil {
			t.Fatal(err)
		}
		session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1))
		}()
		stop, cancel := context.WithTimeout(ctx, time.Second)
		if err = p.Close(stop); err != nil {
			t.Fatal(err)
		}
		cancel()
		wg.Wait()
		waitFor(t, func() bool { return rooms.leaves.Load() == rooms.joins.Load() })
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// viewRooms 按服务端的契约出帧：每确认一帧给下一帧；成员没确认过当前流程版本时，这一帧带上视图。
type viewRooms struct {
	domain.Rooms
	mu       sync.Mutex
	revision uint64 // 流程当前版本，0 表示还没有视图
	members  map[string]*viewMember
	joins    int
}
type viewMember struct{ sequence, acked, pending uint64 }

func (r *viewRooms) Join(_ context.Context, room domain.Ref, binding gateway.Binding, _ uint64) (domain.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.joins++
	token := "token-" + string(rune('0'+r.joins))
	if r.members == nil {
		r.members = map[string]*viewMember{}
	}
	r.members[token] = &viewMember{}
	return domain.Member{Room: room, PlayerID: binding.PlayerID, Token: token, InputGeneration: 1}, nil
}
func (*viewRooms) Submit(context.Context, domain.Member, domain.Input) error { return nil }
func (r *viewRooms) Leave(_ context.Context, m domain.Member) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.members, m.Token)
	return nil
}
func (r *viewRooms) Poll(_ context.Context, m domain.Member, ack uint64) (domain.Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.members[m.Token]
	if v == nil {
		return domain.Delivery{}, domain.ErrMember
	}
	if ack == v.sequence+1 { // 确认了上一次的帧，连同它带的视图
		v.sequence = ack
		if v.pending != 0 {
			v.acked, v.pending = v.pending, 0
		}
	} else if ack != v.sequence {
		return domain.Delivery{}, domain.ErrSequence
	}
	if v.pending == 0 && r.revision > v.acked {
		v.pending = r.revision
	}
	d := domain.Delivery{Room: m.Room, Sequence: v.sequence + 1, Payload: []byte{'f', byte(v.sequence + 1)}}
	if v.pending != 0 {
		d.Level = []byte{'v', byte(v.pending)}
	}
	return d, nil
}
func (r *viewRooms) publish(revision uint64) {
	r.mu.Lock()
	r.revision = revision
	r.mu.Unlock()
}

type push struct {
	id      uint32
	kind    wire.PayloadKind
	payload string
}

// recordingGame 记下每次推送；failLevel 为真时流程视图的推送失败一次。
type recordingGame struct {
	fakeGame
	mu        sync.Mutex
	pushes    []push
	failLevel bool
}

var errPushFailed = errors.New("push failed")

func (g *recordingGame) PushBound(_ context.Context, _ gateway.Binding, id uint32, kind wire.PayloadKind, payload []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failLevel && id == 4 {
		g.failLevel = false
		return errPushFailed
	}
	g.pushes = append(g.pushes, push{id, kind, string(payload)})
	return nil
}
func (g *recordingGame) levels() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, p := range g.pushes {
		if p.id == 4 {
			out = append(out, p.payload)
		}
	}
	return out
}
func (g *recordingGame) frames() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.pushes {
		if p.id == 1 {
			n++
		}
	}
	return n
}

// 流程视图：每个版本推一次，用自己的消息号与 Protobuf 载荷，排在同一次交付的帧之前；推送失败结束这一轮，
// 重连后先拿到最新的一份；没配置消息号时不推；消息号互不相同。
func TestPlayerIngressPushesLevelViewOncePerRevision(t *testing.T) {
	ctx := context.Background()
	seat := func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
		return domain.Ref{ID: "room", Epoch: 1}, 51, nil
	}
	content := func(context.Context, gateway.Binding) (RoomContent, error) { return RoomContent{}, nil }
	for name, cfg := range map[string]PlayerIngressConfig{
		"level = frame":   {InputMessageID: 2, FrameMessageID: 1, Seat: seat, LevelMessageID: 1},
		"level = input":   {InputMessageID: 2, FrameMessageID: 1, Seat: seat, LevelMessageID: 2},
		"level = content": {InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 3, Content: content, LevelMessageID: 3},
	} {
		if _, err := NewPlayerIngress(cfg); !errors.Is(err, domain.ErrRequestInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	start := func(levelMessageID uint32) (*PlayerIngress, *viewRooms, *recordingGame, fakeSession) {
		t.Helper()
		p, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: 2, FrameMessageID: 1, Seat: seat, ContentMessageID: 3, Content: content, LevelMessageID: levelMessageID})
		if err != nil {
			t.Fatal(err)
		}
		binding := ingressBinding()
		rooms, game := &viewRooms{}, &recordingGame{fakeGame: fakeGame{binding: binding}}
		if err = p.Attach(rooms, game); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = p.Close(stop)
		})
		session := fakeSession{id: binding.SessionID, closed: make(chan error, 1)}
		if err = p.OnActive(binding, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Dispatch(ctx, session, 2, 1, wire.PayloadProtobuf, inputPayload(1)); err != nil {
			t.Fatal(err)
		}
		return p, rooms, game, session
	}

	p, rooms, game, session := start(4)
	waitFor(t, func() bool { return game.frames() >= 2 })
	if got := game.levels(); len(got) != 0 {
		t.Fatalf("level view pushed before the flow published one: %q", got)
	}
	rooms.publish(3)
	waitFor(t, func() bool { return len(game.levels()) == 1 })
	seen := game.frames()
	waitFor(t, func() bool { return game.frames() >= seen+3 })
	if got := game.levels(); len(got) != 1 || got[0] != "v\x03" {
		t.Fatalf("revision 3 pushed %d times: %q", len(got), got)
	}
	rooms.publish(4)
	waitFor(t, func() bool { return len(game.levels()) == 2 })
	seen = game.frames()
	waitFor(t, func() bool { return game.frames() >= seen+3 })
	// 逐条核对：视图用消息号 4、Protobuf；帧用消息号 1、Sync；视图紧挨在同一次交付的帧之前；帧号连续不重复。
	game.mu.Lock()
	next := byte(1)
	for i, got := range game.pushes {
		switch got.id {
		case 4:
			if got.kind != wire.PayloadProtobuf || i+1 >= len(game.pushes) || game.pushes[i+1].id != 1 {
				t.Fatalf("push %d: level view %+v is not followed by its frame", i, got)
			}
		case 1:
			if got.kind != wire.PayloadSync || got.payload != string([]byte{'f', next}) {
				t.Fatalf("push %d: frame %q, want sequence %d", i, got.payload, next)
			}
			next++
		default:
			t.Fatalf("push %d: unexpected message %d", i, got.id)
		}
	}
	levels := 0
	for _, got := range game.pushes {
		if got.id == 4 {
			levels++
		}
	}
	game.mu.Unlock()
	if levels != 2 {
		t.Fatalf("%d level pushes for 2 revisions", levels)
	}

	// 视图推送失败：这一次交付的帧不推、不确认，连接关闭。重连后重新 Join，第一帧之前先拿到最新的视图。
	rooms.publish(5)
	game.mu.Lock()
	game.failLevel = true
	game.mu.Unlock()
	select {
	case err := <-session.closed:
		if !errors.Is(err, errPushFailed) {
			t.Fatalf("session closed with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed level push did not close the session")
	}
	game.mu.Lock()
	if last := game.pushes[len(game.pushes)-1]; last.id != 1 {
		t.Fatalf("last push before the failure: %+v", last)
	}
	before := len(game.pushes)
	game.mu.Unlock()
	p.OnClosed(ingressBinding(), 1)
	if err := p.OnActive(ingressBinding(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Dispatch(ctx, session, 2, 2, wire.PayloadProtobuf, inputPayload(1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		game.mu.Lock()
		defer game.mu.Unlock()
		return len(game.pushes) >= before+2
	})
	game.mu.Lock()
	first, second := game.pushes[before], game.pushes[before+1]
	game.mu.Unlock()
	if first != (push{4, wire.PayloadProtobuf, "v\x05"}) || second != (push{1, wire.PayloadSync, "f\x01"}) {
		t.Fatalf("after reconnect: %+v then %+v, want the latest view then frame 1", first, second)
	}
	rooms.mu.Lock()
	joins := rooms.joins
	rooms.mu.Unlock()
	if joins != 2 {
		t.Fatalf("joins=%d", joins)
	}

	// 没配置流程消息号：帧照常推，视图不推。
	_, quiet, silent, _ := start(0)
	quiet.publish(9)
	waitFor(t, func() bool { return silent.frames() >= 3 })
	silent.mu.Lock()
	for _, got := range silent.pushes {
		if got.id != 1 || got.kind != wire.PayloadSync {
			t.Fatalf("pushed %+v without a level message id; only frames are expected", got)
		}
	}
	silent.mu.Unlock()
}
