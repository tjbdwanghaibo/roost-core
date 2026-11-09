package cellworld

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/terrain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// extInbox 收集一个命名空间的扩展消息（处理器在传输回调上，只复制、不阻塞）。
type extInbox struct {
	mu   sync.Mutex
	got  []extDelivery
	cond chan struct{}
}

type extDelivery struct {
	from int32
	body []byte
}

func newExtInbox() *extInbox { return &extInbox{cond: make(chan struct{}, 1)} }

func (in *extInbox) handle(from int32, body []byte) {
	in.mu.Lock()
	in.got = append(in.got, extDelivery{from: from, body: bytes.Clone(body)})
	in.mu.Unlock()
	select {
	case in.cond <- struct{}{}:
	default:
	}
}

func (in *extInbox) waitFor(t *testing.T, n int) []extDelivery {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		in.mu.Lock()
		if len(in.got) >= n {
			out := append([]extDelivery(nil), in.got...)
			in.mu.Unlock()
			return out
		}
		in.mu.Unlock()
		select {
		case <-in.cond:
		case <-deadline:
			t.Fatalf("timed out waiting for %d ext messages", n)
		}
	}
}

// v7 扩展消息：发给别的进程与本进程自己的消息原样交给对方命名空间的处理器、from 是发件 sid；没有处理器的命名空间计
// ExtDrops；CloseIngress 之后处理器不再被调用；旧版本（v6）的外壳与没有内容的扩展消息按坏消息丢弃。
func TestExtMessagesReachTheHandlerOfTheirSpace(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	transport := newLoopback()
	inboxA, inboxB := newExtInbox(), newExtInbox()
	cellA, cellB := CellID{0, 0}, CellID{1, 0}
	a := startProc(t, procSpec{sid: grid.SID(cellA), cell: &cellA, grid: grid, transport: transport, mod: func(cfg *NodeConfig) {
		cfg.Ext = map[string]ExtHandler{"terrain": inboxA.handle}
	}})
	defer a.stop()
	b := startProc(t, procSpec{sid: grid.SID(cellB), cell: &cellB, grid: grid, transport: transport, mod: func(cfg *NodeConfig) {
		cfg.Ext = map[string]ExtHandler{"terrain": inboxB.handle}
	}})
	defer b.stop()

	if !a.node.SendExt(b.sid, "terrain", []byte("to b")) || !a.node.SendExt(a.sid, "terrain", []byte("to self")) {
		t.Fatal("SendExt refused a valid message")
	}
	if a.node.SendExt(b.sid, "bad space", []byte("x")) || a.node.SendExt(0, "terrain", []byte("x")) {
		t.Fatal("SendExt accepted an invalid space or sid 0")
	}
	if got := inboxB.waitFor(t, 1); got[0].from != a.sid || string(got[0].body) != "to b" {
		t.Fatalf("b got %+v, want one message from sid %d", got, a.sid)
	}
	if got := inboxA.waitFor(t, 1); got[0].from != a.sid || string(got[0].body) != "to self" {
		t.Fatalf("a got %+v, want its own message", got)
	}

	// 没有处理器的命名空间：丢弃并计数。
	a.node.SendExt(b.sid, "buildings", []byte("nobody"))
	deadline := time.Now().Add(5 * time.Second)
	for b.node.Stats().ExtDrops != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("ext drops %d, want 1", b.node.Stats().ExtDrops)
		}
		time.Sleep(time.Millisecond)
	}

	// 提交后回调里发：SendExt 只入队（不 Dispatch），在 Nest 的提交后回调里同样可用——这里用 commit()（不在事务里时
	// 立即执行，在事务里时登记为提交后回调）走同一条路。
	commit(func() { a.node.SendExt(b.sid, "terrain", []byte("after commit")) })
	if got := inboxB.waitFor(t, 2); string(got[1].body) != "after commit" {
		t.Fatalf("b got %q second, want the message sent after the commit", got[1].body)
	}

	// CloseIngress 之后：处理器不再被调用。
	if err := b.node.CloseIngress(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.node.SendExt(b.sid, "terrain", []byte("late"))
	time.Sleep(50 * time.Millisecond)
	if got := len(inboxB.waitFor(t, 2)); got != 2 {
		t.Fatalf("b's handler ran %d times after CloseIngress, want still 2", got)
	}

	// 旧版本的外壳（v6）按坏消息丢弃，不进处理器。
	old, err := msgpack.Marshal(&wireMsg{V: 6, Kind: kindExt, From: a.sid, Ext: &extMsg{Space: "terrain", Body: []byte("v6")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeMsg(old); !errors.Is(err, errBadMessage) {
		t.Fatalf("a v6 ext message decoded: %v", err)
	}
	if _, err := decodeMsg(mustEncode(t, &wireMsg{Kind: kindExt, From: a.sid})); !errors.Is(err, errBadMessage) {
		t.Fatalf("an ext message without a body decoded: %v", err)
	}
}

func mustEncode(t *testing.T, m *wireMsg) []byte {
	t.Helper()
	data, err := encodeMsg(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// NodeConfig.Ext 的命名空间与 world 同一套字符规则，处理器必填。
func TestExtSpacesAreValidated(t *testing.T) {
	grid, err := NewGrid(GridConfig{Cols: 1, Rows: 1, CellSize: 1000, GhostBand: 100, Hysteresis: 10, Owners: map[CellID]int32{{}: 1}})
	if err != nil {
		t.Fatal(err)
	}
	base := NodeConfig{World: "w", SID: 1, Grid: grid, Transport: newLoopback(),
		Nest:    nest.NewEngine(nest.NestOptionWithGetter(fentity.NewManagerAccess(fentity.NewEntityManager()))),
		Destroy: func(context.Context, fentity.IThreadSafeEntity) error { return nil },
		Cell:    &CellID{}, Scene: mustID(t, 1, kindTScene), ViewRadius: 50,
		CreateActor: func(int64, EntityKind) (Actor, error) { return nil, errors.New("unused") }}
	for _, ext := range []map[string]ExtHandler{
		{"has space": func(int32, []byte) {}},
		{"": func(int32, []byte) {}},
		{"terrain": nil},
	} {
		cfg := base
		cfg.Ext = ext
		if _, err := NewNode(cfg); !errors.Is(err, ErrNodeConfig) {
			t.Fatalf("ext %v accepted: %v", ext, err)
		}
	}
}

// ---- 扩展消息的第一个使用方：可编辑地形的边界块副本（gameplay/terrain 的 Replicator）----

// replicaViewStore 是副本一侧的存放处：一张视图（与 game-demo 的格进程把 Ghost 带的块装进 terrainmap 视图同一规则：
// 新版本覆盖、拥有者说“原始块”时按原始块装入、本地更新的版本不回退）。
type replicaViewStore struct {
	mu   sync.Mutex
	view *terrain.View
}

func (s *replicaViewStore) Held(key terrain.ChunkKey) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version, loaded := s.view.Version(key); loaded {
		return version
	}
	return -1
}

func (s *replicaViewStore) Apply(key terrain.ChunkKey, o terrain.Overlay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.Version >= 1 {
		return s.view.SetOverlay(key, o)
	}
	if version, loaded := s.view.Version(key); loaded && version >= 1 {
		return terrain.ErrStaleOverlay
	}
	return s.view.LoadBase(key)
}

func (s *replicaViewStore) Forget(terrain.ChunkKey) {}

// terrainCell 是一个格进程里与地形有关的部分：视图（整张图装着）、Replicator（拥有者一侧以自己的视图为源，副本一侧
// 订阅邻格写的 Ghost 带块），消息经本进程的 cellworld Node 的扩展消息收发。
type terrainCell struct {
	proc     *testProc
	view     *terrain.View
	rep      *terrain.Replicator
	cancel   context.CancelFunc
	done     chan struct{}
	applied  chan terrainApplied
	keysMine func(terrain.ChunkKey) bool
}

type terrainApplied struct {
	key     terrain.ChunkKey
	version int64
}

// terrainGrid：2 × 1 个格，格边长 2 块（12800），Ghost 带 1 块；地图 4 × 1 块：块 x 0、1 归格 (0,0)，2、3 归格 (1,0)。
func terrainGrid(t testing.TB) *Grid {
	t.Helper()
	g, err := NewGrid(GridConfig{CellSize: 2 * terrain.ChunkSize, Cols: 2, Rows: 1, GhostBand: terrain.ChunkSize, Hysteresis: 100,
		Owners: map[CellID]int32{{0, 0}: 101, {1, 0}: 102}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func chunkOwner(grid *Grid) func(terrain.ChunkKey) (int32, bool) {
	return func(key terrain.ChunkKey) (int32, bool) {
		cell, ok := grid.CellAt(key.Bounds().Min)
		if !ok {
			return 0, false
		}
		return grid.SID(cell), true
	}
}

func startTerrainCell(t testing.TB, grid *Grid, transport Transport, pack *terrain.Pack, cell CellID) *terrainCell {
	t.Helper()
	sid := grid.SID(cell)
	tc := &terrainCell{view: terrain.NewView(pack), applied: make(chan terrainApplied, 1024)}
	for key := range terrain.ChunksIn(pack.Meta().Bounds()) {
		if err := tc.view.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
	owner := chunkOwner(grid)
	tc.keysMine = func(key terrain.ChunkKey) bool { o, ok := owner(key); return ok && o == sid }
	var node *Node
	rep, err := terrain.NewReplicator(terrain.ReplicatorConfig{
		Self: sid, Meta: pack.Meta(), OwnerOf: owner, Source: tc.view, Owns: tc.keysMine,
		Send:  func(to int32, body []byte) bool { return node.SendExt(to, "terrain", body) },
		Renew: 200 * time.Millisecond,
		Applied: func(_ terrain.ReplicaChannel, key terrain.ChunkKey, version int64, _ time.Duration) {
			tc.applied <- terrainApplied{key: key, version: version}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.AddChannel(1, &replicaViewStore{view: tc.view}); err != nil {
		t.Fatal(err)
	}
	tc.rep = rep
	tc.proc = startProc(t, procSpec{sid: sid, cell: &cell, grid: grid, transport: transport, mod: func(cfg *NodeConfig) {
		cfg.Ext = map[string]ExtHandler{"terrain": func(from int32, body []byte) { rep.Deliver(from, body) }}
	}})
	node = tc.proc.node
	ctx, cancel := context.WithCancel(context.Background())
	tc.cancel, tc.done = cancel, make(chan struct{})
	go func() { defer close(tc.done); _ = rep.Run(ctx) }()
	// Ghost 带：本格外扩 GhostBand 里、不归本格写的块。
	r := grid.Rect(cell)
	band := spatial.Rect{Min: spatial.Point{X: r.Min.X - grid.GhostBand(), Y: r.Min.Y - grid.GhostBand()},
		Max: spatial.Point{X: r.Max.X + grid.GhostBand(), Y: r.Max.Y + grid.GhostBand()}}
	var ghost []terrain.ChunkKey
	for key := range terrain.ChunksIn(band) {
		if pack.Meta().Contains(key) && !tc.keysMine(key) {
			ghost = append(ghost, key)
		}
	}
	rep.Want(1, ghost)
	return tc
}

func (tc *terrainCell) stop() {
	tc.cancel()
	<-tc.done
	tc.proc.stop()
}

// commit 是拥有者一次改块的提交：新版本进视图，然后 Changed（与 game-demo 的提交后回调同一顺序；这里在 commit()
// 里调用，走与 Nest 提交后回调相同的路：只登记、不阻塞）。返回改完的块。
func (tc *terrainCell) commit(t testing.TB, key terrain.ChunkKey, version int64, lift uint16) terrain.Chunk {
	t.Helper()
	c, _, _ := tc.view.Chunk(key)
	cx, cy := int(version*7)%54+5, int(version*13)%54+5
	for j := cy - 5; j <= cy+5; j++ {
		for i := cx - 5; i <= cx+5; i++ {
			if (i-cx)*(i-cx)+(j-cy)*(j-cy) <= 25 {
				c.Heights[j*terrain.ChunkSamples+i] += lift // r=5m 的抬高，与玩家小笔刷同样的改法
			}
		}
	}
	if err := tc.view.SetOverlay(key, terrain.Overlay{BaseRevision: tc.view.Meta().BaseRevision, Version: version, Heights: c.Heights, Materials: c.Materials}); err != nil {
		t.Fatal(err)
	}
	commit(func() { tc.rep.Changed(key) })
	return c
}

// waitApplied 等副本应用到 key 的 version，返回从调用起的耗时。
func (tc *terrainCell) waitApplied(t testing.TB, key terrain.ChunkKey, version int64) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case a := <-tc.applied:
			if a.key == key && a.version >= version {
				return time.Since(start)
			}
		case <-deadline:
			t.Fatalf("chunk %v did not reach version %d on sid %d: %+v", key, version, tc.proc.sid, tc.rep.Stats())
		}
	}
}

// 边界块副本跑在真实的 cellworld Node 上（进程内回环与真实 nats-server 各一遍）：格 (0,0) 改它边上的块 (1,0)，格 (1,0)
// 的 Ghost 带副本在有界时间内到同一版本、逐点相同的数据；同一块连改几次停在最新版本；本格自己的块不订阅；消息只走
// 扩展消息（NodeStats 的 ExtOut / ExtIn）。ROOST_TERRAIN_REPLICA_PERF=1 时多改几百次，记下提交到副本应用完的延迟与每块推送字节。
func TestTerrainReplicaOverNodes(t *testing.T) {
	for _, tr := range []struct {
		name      string
		transport func(t *testing.T) func() Transport
	}{
		{"loopback", func(*testing.T) func() Transport { l := newLoopback(); return func() Transport { return l } }},
		{"nats", func(t *testing.T) func() Transport {
			url := startNATSServer(t)
			return func() Transport { return NATSTransport(dialNATS(t, url)) }
		}},
	} {
		t.Run(tr.name, func(t *testing.T) {
			transport := tr.transport(t)
			grid := terrainGrid(t)
			meta := terrain.Meta{BaseRevision: 1, ChunksX: 4, ChunksY: 1, WaterLevel: 40000}
			path := filepath.Join(t.TempDir(), "replica.pack")
			if err := terrain.WritePackFile(path, meta, terrain.Generated(meta, 7)); err != nil {
				t.Fatal(err)
			}
			pack, err := terrain.OpenPack(path)
			if err != nil {
				t.Fatal(err)
			}
			defer pack.Close()
			a := startTerrainCell(t, grid, transport(), pack, CellID{0, 0})
			defer a.stop()
			b := startTerrainCell(t, grid, transport(), pack, CellID{1, 0})
			defer b.stop()
			border := terrain.ChunkKey{X: 1, Y: 0} // 格 (0,0) 写，在格 (1,0) 的 Ghost 带里
			deadline := time.Now().Add(5 * time.Second)
			for a.rep.Stats().Subscribers != 1 || b.rep.Stats().Subscribers != 1 {
				if time.Now().After(deadline) {
					t.Fatalf("subscriptions: a %+v, b %+v", a.rep.Stats(), b.rep.Stats())
				}
				time.Sleep(time.Millisecond)
			}
			for v := int64(1); v <= 3; v++ {
				a.commit(t, border, v, uint16(10*v))
			}
			b.waitApplied(t, border, 3)
			ca, va, _ := a.view.Chunk(border)
			cb, vb, _ := b.view.Chunk(border)
			if va != 3 || vb != 3 || !slices.Equal(ca.Heights, cb.Heights) || !bytes.Equal(ca.Materials, cb.Materials) {
				t.Fatalf("owner version %d, replica version %d, same data %v", va, vb, slices.Equal(ca.Heights, cb.Heights))
			}
			for x := border.Bounds().Min.X; x < border.Bounds().Max.X; x += 97 {
				ha, errA := a.view.Height(x, 3000)
				hb, errB := b.view.Height(x, 3000)
				if errA != nil || errB != nil || ha != hb {
					t.Fatalf("height at (%d, 3000): owner %d %v, replica %d %v", x, ha, errA, hb, errB)
				}
			}
			if s := a.proc.node.Stats(); s.ExtOut == 0 || s.ExtIn == 0 {
				t.Fatalf("node stats %+v: the replica did not go over the ext messages", s)
			}
			if s := b.rep.Stats(); s.DecodeErrors != 0 || s.ApplyErrors != 0 || s.WrongOwner != 0 || s.BaseRevisionDrops != 0 {
				t.Fatalf("replica stats %+v", s)
			}

			if os.Getenv("ROOST_TERRAIN_REPLICA_PERF") != "1" {
				return
			}
			const n = 300
			latencies := make([]time.Duration, 0, n)
			before := a.rep.Stats()
			for v := int64(4); v < 4+n; v++ {
				start := time.Now()
				a.commit(t, border, v, uint16(v%50))
				b.waitApplied(t, border, v)
				latencies = append(latencies, time.Since(start))
			}
			after := a.rep.Stats()
			slices.Sort(latencies)
			t.Logf("%s: commit → neighbour replica applied (r=5m raise on a generated chunk): n=%d p50=%v p90=%v p99=%v max=%v; push bytes per chunk %.0f (chunk messages %d, bytes %d)",
				tr.name, n, latencies[n/2], latencies[n*9/10], latencies[n*99/100], latencies[n-1],
				float64(after.ChunkBytesOut-before.ChunkBytesOut)/float64(after.ChunksOut-before.ChunksOut),
				after.ChunksOut-before.ChunksOut, after.ChunkBytesOut-before.ChunkBytesOut)
		})
	}
}

// ---- 跨格移动按高度判定（NodeConfig.Surface 接 gameplay/terrain 的 Walker，可编辑地形 P3 / T06）----

// flatPack 写一张平地（高度原始值 1000 = 100 米、没有水）的 chunksX × 1 块地图。
func flatPack(t testing.TB, chunksX int32) *terrain.Pack {
	t.Helper()
	meta := terrain.Meta{BaseRevision: 1, ChunksX: chunksX, ChunksY: 1, WaterLevel: math.MinInt32}
	path := filepath.Join(t.TempDir(), "flat.pack")
	if err := terrain.WritePackFile(path, meta, func(terrain.ChunkKey) (terrain.Chunk, error) {
		c := terrain.NewChunk()
		for i := range c.Heights {
			c.Heights[i] = 1000
		}
		return c, nil
	}); err != nil {
		t.Fatal(err)
	}
	pack, err := terrain.OpenPack(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pack.Close() })
	return pack
}

// setColumns 把视图里 key 块上世界采样列 gx ∈ cols 的整列（gy 0～63）改成高度原始值 raw，装成版本 + 1 的覆盖块
// （拥有者一次改块的提交），返回新版本。
func setColumns(t testing.TB, view *terrain.View, key terrain.ChunkKey, cols []int64, raw uint16) int64 {
	t.Helper()
	c, version, loaded := view.Chunk(key)
	if !loaded {
		t.Fatalf("chunk %v not loaded", key)
	}
	for _, gx := range cols {
		for gy := range int64(terrain.ChunkSamples) {
			c.Heights[gy*terrain.ChunkSamples+gx-int64(key.X)*terrain.ChunkSamples] = raw
		}
	}
	if err := view.SetOverlay(key, terrain.Overlay{BaseRevision: view.Meta().BaseRevision, Version: version + 1, Heights: c.Heights, Materials: c.Materials}); err != nil {
		t.Fatal(err)
	}
	return version + 1
}

// surfaceCellGrid：2 × 1 个格，格边长 2 块（12800），Ghost 带 1 块，滞回 1000（冻结线离边界 10 米，交接前在邻格的块上
// 走得足够远）。地图 4 × 1 块：块 0、1 归格 (0,0)（sid 101），2、3 归格 (1,0)（sid 102）。格 (0,0) 的视图装块 0～2
// （块 2 是 Ghost 带里邻格写的块，副本），格 (1,0) 装块 1～3。
func surfaceCellGrid(t testing.TB) *Grid {
	t.Helper()
	g, err := NewGrid(GridConfig{CellSize: 2 * terrain.ChunkSize, Cols: 2, Rows: 1, GhostBand: terrain.ChunkSize, Hysteresis: 1000,
		Owners: map[CellID]int32{{0, 0}: 101, {1, 0}: 102}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// 副本缺块不猜平地：格 (0,0) 的视图里邻格写的块 2 没装入（副本装不进来）时，朝东走的玩家停在块 2 前最后一个点
// （节点 127 的最后一个点 12749，节点 128 从 12750 起就在块 2 里），NPC 原路折回；块 2 的副本装入后同样的走法走进去；
// 站在没装入的块上的角色一步不走。用真实的 terrain.Walker 与视图，Node 只积分（不起传输）。
func TestCrossCellWalkDoesNotGuessAMissingGhostChunk(t *testing.T) {
	grid := surfaceCellGrid(t)
	pack := flatPack(t, 4)
	view := terrain.NewView(pack)
	for _, key := range []terrain.ChunkKey{{X: 0}, {X: 1}} {
		if err := view.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
	walker := &terrain.Walker{View: view, Rules: terrain.DefaultWalkRules}
	cell := CellID{0, 0}
	n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, ViewRadius: worldViewRadius, Surface: func() Surface { return walker }}.withDefaults(), cell: cell, hostsCell: true}
	n.rect = grid.Rect(cell)
	now := time.Now()

	player := ActorState{EntityState: EntityState{ID: actorID(1), Kind: KindPlayer, X: 12500, Y: 3000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&player, now, 4*worldPeriod)
	if player.X != 12749 || player.VX != 0 || player.Role != RoleReal {
		t.Fatalf("walking towards the missing ghost chunk: at x=%d vx %d %v, want stopped at 12749", player.X, player.VX, player.Role)
	}
	// NPC 在第二个子步碰上、原路折回；同一拍剩下的子步往回走（与碰世界边缘反弹相同）。
	npc := ActorState{EntityState: EntityState{ID: npcID(2), Kind: KindNPC, X: 12600, Y: 3000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&npc, now, 2*worldPeriod)
	if npc.X != 12749 || npc.VX != -2000 {
		t.Fatalf("npc towards the missing ghost chunk: at x=%d vx %d, want turned back at 12749", npc.X, npc.VX)
	}
	n.stepActor(&npc, now, worldPeriod)
	if npc.X != 12649 {
		t.Fatalf("npc after turning back: at x=%d, want 12649", npc.X)
	}
	inside := ActorState{EntityState: EntityState{ID: actorID(3), Kind: KindPlayer, X: 13000, Y: 3000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&inside, now, worldPeriod)
	if inside.X != 13000 || inside.VX != 0 {
		t.Fatalf("standing on the missing ghost chunk: at x=%d vx %d, want held", inside.X, inside.VX)
	}

	if err := view.LoadBase(terrain.ChunkKey{X: 2}); err != nil { // 副本到了（拥有者答“原始块”）
		t.Fatal(err)
	}
	player.VX = 2000
	n.stepActor(&player, now, 4*worldPeriod)
	if player.X != 13149 || player.VX != 2000 {
		t.Fatalf("after the ghost chunk arrived: at x=%d vx %d, want walked on to 13149", player.X, player.VX)
	}
}

// 跨格移动按邻格的地形（真实 Node、回环传输、两个格进程各自的视图与 terrain.Replicator、Base 与玩家）：
//
//  1. 格 (1,0) 在它写的块 2 上立一道 10 米的墙（节点 133～134），推到格 (0,0) 的 Ghost 副本；格 (0,0) 的玩家朝东走，
//     越过格边界（12800）后在墙前停下（13249），还没到冻结线（13800），一直是格 (0,0) 的 Real：交接之前格 (0,0) 按
//     副本判定。
//  2. 格 (1,0) 把墙挖平（新版本推到副本），玩家再走：越过冻结线交给格 (1,0)。
//  3. 格 (1,0) 在只有它装着的块 3 上挖一个 10 米深的坑（节点 195 起），玩家停在坑前（19449）：交接之后按新格自己的
//     视图判定（格 (0,0) 的视图里没有块 3，要是它在判定会停在 19149）。
func TestCrossCellWalkFollowsTheNeighboursTerrain(t *testing.T) {
	grid := surfaceCellGrid(t)
	pack := flatPack(t, 4)
	lb := newLoopback()
	type surfaceCell struct {
		view *terrain.View
		rep  *terrain.Replicator
		node atomic.Pointer[Node]
	}
	cells := map[CellID]*surfaceCell{}
	owner := chunkOwner(grid)
	for _, id := range grid.Cells() {
		sid := grid.SID(id)
		sc := &surfaceCell{view: terrain.NewView(pack)}
		r := grid.Rect(id)
		load := spatial.Rect{Min: spatial.Point{X: r.Min.X - grid.GhostBand(), Y: r.Min.Y - grid.GhostBand()},
			Max: spatial.Point{X: r.Max.X + grid.GhostBand(), Y: r.Max.Y + grid.GhostBand()}}
		var ghost []terrain.ChunkKey
		for key := range terrain.ChunksIn(load) {
			if !pack.Meta().Contains(key) {
				continue
			}
			if err := sc.view.LoadBase(key); err != nil {
				t.Fatal(err)
			}
			if o, _ := owner(key); o != sid {
				ghost = append(ghost, key)
			}
		}
		rep, err := terrain.NewReplicator(terrain.ReplicatorConfig{
			Self: sid, Meta: pack.Meta(), OwnerOf: owner, Source: sc.view,
			Owns:  func(key terrain.ChunkKey) bool { o, ok := owner(key); return ok && o == sid },
			Send:  func(to int32, body []byte) bool { return sc.node.Load().SendExt(to, "terrain", body) },
			Renew: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := rep.AddChannel(1, &replicaViewStore{view: sc.view}); err != nil {
			t.Fatal(err)
		}
		rep.Want(1, ghost)
		sc.rep = rep
		cells[id] = sc
	}
	spec := worldSpec{grid: grid, transport: func(testing.TB, string) Transport { return lb }, cellMod: func(cfg *NodeConfig) {
		sc := cells[*cfg.Cell]
		walker := &terrain.Walker{View: sc.view, Rules: terrain.DefaultWalkRules}
		cfg.Surface = func() Surface { return walker }
		cfg.Ext = map[string]ExtHandler{"terrain": func(from int32, body []byte) { sc.rep.Deliver(from, body) }}
	}}
	w := startWorld(t, spec)
	ctx, cancel := context.WithCancel(context.Background())
	var reps sync.WaitGroup
	for id, sc := range cells {
		sc.node.Store(w.cells[id].node)
		reps.Go(func() { _ = sc.rep.Run(ctx) })
	}
	t.Cleanup(func() { cancel(); reps.Wait() })
	a, b := cells[CellID{0, 0}], cells[CellID{1, 0}]
	waitFor(t, 5*time.Second, "the ghost subscriptions", func() bool { return a.rep.Stats().Subscribers == 1 && b.rep.Stats().Subscribers == 1 })

	ghostChunk, farChunk := terrain.ChunkKey{X: 2}, terrain.ChunkKey{X: 3}
	wall := setColumns(t, b.view, ghostChunk, []int64{133, 134}, 1100)
	commit(func() { b.rep.Changed(ghostChunk) })
	waitFor(t, 5*time.Second, "the wall on the ghost replica", func() bool { v, _ := a.view.Version(ghostChunk); return v == wall })

	const player = 61
	w.enter(player, spatial.Point{X: 12500, Y: 3000})
	w.move(player, 2000, 0)
	v := w.waitView(player, 5*time.Second, "stopped at the wall", func(v View) bool { return v.Self.X == 13249 && v.Self.VX == 0 })
	if v.Cell != 101 {
		t.Fatalf("stopped at the wall on cell sid %d, want 101 (before the freeze line, on the ghost replica)", v.Cell)
	}
	if s := w.cells[CellID{0, 0}].node.Stats(); s.SurfaceBlocks == 0 || s.HandoffsStarted != 0 {
		t.Fatalf("cell (0,0) stats %+v: want a surface block and no handoff", s)
	}

	flat := setColumns(t, b.view, ghostChunk, []int64{133, 134}, 1000)
	commit(func() { b.rep.Changed(ghostChunk) })
	waitFor(t, 5*time.Second, "the levelled wall on the ghost replica", func() bool { v, _ := a.view.Version(ghostChunk); return v == flat })
	pit := setColumns(t, b.view, farChunk, []int64{195, 196}, 900)
	commit(func() { b.rep.Changed(farChunk) })
	if _, loaded := a.view.Version(farChunk); loaded {
		t.Fatal("cell (0,0) holds chunk 3; the last leg cannot tell whose view judged it")
	}
	w.move(player, 2000, 0)
	v = w.waitView(player, 10*time.Second, "stopped at the pit", func(v View) bool { return v.Self.X == 19449 && v.Self.VX == 0 })
	if v.Cell != 102 {
		t.Fatalf("stopped at the pit on cell sid %d, want 102 (handed off)", v.Cell)
	}
	if got, _ := b.view.Version(farChunk); got != pit {
		t.Fatalf("chunk 3 on cell (1,0) at version %d, want %d", got, pit)
	}
	w.assertSingleReal(actorID(player), CellID{1, 0})
}

// 一拍积分的成本（一个 Period、无交接）：没有地面、平地上的 terrain.Walker；玩家全速（2000 / 秒，一拍 100 个点）
// 与慢 NPC（200 / 秒，一拍 10 个点）。
func BenchmarkIntegrateOnSurface(b *testing.B) {
	grid := surfaceCellGrid(b)
	view := terrain.NewView(flatPack(b, 4))
	for key := range terrain.ChunksIn(view.Meta().Bounds()) {
		if err := view.LoadBase(key); err != nil {
			b.Fatal(err)
		}
	}
	walker := &terrain.Walker{View: view, Rules: terrain.DefaultWalkRules}
	cell := CellID{0, 0}
	for _, ground := range []struct {
		name    string
		surface func() Surface
	}{{"none", nil}, {"walker", func() Surface { return walker }}} {
		n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, ViewRadius: worldViewRadius, Surface: ground.surface}.withDefaults(), cell: cell, hostsCell: true}
		n.rect = grid.Rect(cell)
		for _, speed := range []int64{2000, 200} {
			b.Run(fmt.Sprintf("%s/speed%d", ground.name, speed), func(b *testing.B) {
				st := ActorState{EntityState: EntityState{ID: npcID(1), Kind: KindNPC, X: 1000, Y: 3000, VX: speed, HP: 100}, Role: RoleReal}
				var fx actorEffects
				for b.Loop() {
					if st.X > 11000 {
						st.X = 1000
					}
					n.advance(&st, 0, worldPeriod, &fx)
				}
			})
		}
	}
}
