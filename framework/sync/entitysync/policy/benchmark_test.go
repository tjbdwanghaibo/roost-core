package policy

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 批量真实重算：空间移动与覆盖更新分别计时；不包含网络与初始化。
func BenchmarkAOIBatchRecompute(b *testing.B) {
	for _, dense := range []bool{false, true} {
		for _, coverage := range []bool{false, true} {
			for _, workers := range []int{1, 4} {
				b.Run(fmt.Sprintf("dense=%v/coverage=%v/workers=%d", dense, coverage, workers), func(b *testing.B) {
					a, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 4000, Y: 4000}}, BlockSize: 100, EnterRadius: 120, LeaveRadius: 150, MaxVisible: 64})
					if err != nil {
						b.Fatal(err)
					}
					check := func(err error) {
						if err != nil {
							b.Fatal(err)
						}
					}
					spacing := int64(100)
					if dense {
						spacing = 10
					}
					positions := make([]spatial.Point, 512)
					for i := range positions {
						positions[i] = spatial.Point{X: 500 + int64(i%32)*spacing, Y: 500 + int64(i/32)*spacing}
						check(a.AddSubject(int64(i+1), positions[i]))
					}
					for i := range 128 {
						check(a.AddObserver(int64(i+1000), int64(i+1000), positions[i*4], ObserverView{}))
					}
					a.Flush()
					a.FlushBlocks()
					step := 0
					b.ReportAllocs()
					for b.Loop() {
						a.beginBatch()
						for i := range 32 {
							id := int64(i + 1)
							if coverage {
								var blocks []int64
								if step%2 == 0 {
									blocks = []int64{a.BlockAt(positions[i]), a.BlockAt(spatial.Point{X: positions[i].X + 100, Y: positions[i].Y})}
								}
								check(a.SetObservedBlocks(id, blocks))
							} else {
								at := positions[i]
								at.X += int64(step%2) * 80
								check(a.MoveSubject(id, at))
							}
						}
						a.endBatch(workers)
						a.Flush()
						a.FlushBlocks()
						step++
					}
				})
			}
		}
	}
}

// 多视野密集场景的一个政策批次（sync-aoi -views-per-player 16 按 1/4 缩小）：2500 个目标、4250 个容量饱和的空间视野，
// 每批 1% 的目标小步移动，其中跟随它们的视野（每个目标约 1.7 个）一起移动。只量 AOI 批次与事件收集，不含 Interest 与 Manager。
func BenchmarkAOIBatchDenseViews(b *testing.B) {
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			const side, observers, moves = 50, 4250, 25
			a, positions := denseSaturatedAOI(b, side, observers)
			followers := make(map[int][]int64)
			for i := range observers {
				followers[1+(i*7919)%(side*side)] = append(followers[1+(i*7919)%(side*side)], int64(100_000+i))
			}
			step, scans, dirtied := 0, a.fullScans, 0
			b.ReportAllocs()
			for b.Loop() {
				a.beginBatch()
				for k := range moves {
					id := 1 + ((step*moves+k)*7919)%(side*side)
					at := positions[id]
					at.X += 3 - int64(step%2)*6
					positions[id] = at
					if err := a.MoveSubject(int64(id), at); err != nil {
						b.Fatal(err)
					}
					for _, observer := range followers[id] {
						if err := a.MoveObserver(observer, at); err != nil {
							b.Fatal(err)
						}
					}
				}
				dirtied += len(a.dirty)
				a.endBatch(workers)
				a.Flush()
				a.FlushBlocks()
				step++
			}
			// 每批被波及的观察者数与其中完整重评的次数（其余只评估移动过的目标）。
			b.ReportMetric(float64(dirtied)/float64(step), "dirtied/op")
			b.ReportMetric(float64(a.fullScans-scans)/float64(step), "fullscans/op")
		})
	}
}

// 空闲与稀疏变化分别测量事件收集成本；不把地图初始化计入每轮成本。
func BenchmarkAOIEventCollection(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, move := range []bool{false, true} {
			b.Run(fmt.Sprintf("observers=%d/move=%v", count, move), func(b *testing.B) {
				aoi, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: int64(count)*1000 + 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 50, LeaveRadius: 60})
				if err != nil {
					b.Fatal(err)
				}
				for i := 1; i <= count; i++ {
					if err := aoi.AddObserver(int64(i), int64(i), spatial.Point{X: int64(i) * 1000, Y: 100}, ObserverView{}); err != nil {
						b.Fatal(err)
					}
				}
				if err := aoi.AddSubject(-1, spatial.Point{X: 1000, Y: 100}); err != nil {
					b.Fatal(err)
				}
				aoi.Flush()
				aoi.FlushBlocks()
				b.ReportAllocs()
				step := 0
				for b.Loop() {
					if move {
						x := int64(1000 + (step%2)*100)
						if err := aoi.MoveSubject(-1, spatial.Point{X: x, Y: 100}); err != nil {
							b.Fatal(err)
						}
						step++
					}
					aoi.Flush()
					aoi.FlushBlocks()
				}
			})
		}
	}
}

// ---- 视野：只测登记表与应用侧扇出（drainLogLocked / drainAOILocked 写进 held），不含 Manager 订阅与打包 ----

func benchmarkInterest(b *testing.B) *Interest {
	b.Helper()
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = manager.Close(context.Background()) })
	// 基准只量登记表与扇出，视野数要到 64，所以用不受视野上限约束的异步模式。
	in, err := NewInterest(InterestConfig{Manager: manager, ApplyMode: ApplyAsync, MaxPlayersPerView: 1 << 20, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 100_000, Y: 100_000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(in.Close)
	return in
}

// settleViews 把登记表的变更与 AOI 事件写进 held，再丢掉结算标记：基准只量扇出，不量 Manager。
func settleViews(in *Interest) {
	in.mu.Lock()
	in.drainLogLocked()
	in.drainAOILocked()
	clear(in.touched)
	in.mu.Unlock()
}

// 共享关系视野（联盟）每轮换一个成员：成本 = 变化成员数 × 挂接玩家数。
func BenchmarkRelationViewMemberChange(b *testing.B) {
	for _, players := range []int{1, 64, 1024} {
		for _, members := range []int{32, 1024} {
			b.Run(fmt.Sprintf("players=%d/members=%d", players, members), func(b *testing.B) {
				in := benchmarkInterest(b)
				const id = ViewID(1)
				if err := in.DefineRelationView(id, entity.SyncProfile{}); err != nil {
					b.Fatal(err)
				}
				ids := make([]int64, members)
				for i := range ids {
					ids[i] = int64(i + 1)
				}
				if err := in.SetViewMembers(id, ids); err != nil {
					b.Fatal(err)
				}
				for player := 1; player <= players; player++ {
					if err := in.AttachView(int64(1_000_000+player), id); err != nil {
						b.Fatal(err)
					}
				}
				settleViews(in)
				b.ReportAllocs()
				spare := int64(members + 1)
				for b.Loop() {
					if err := in.AddViewMembers(id, spare); err != nil {
						b.Fatal(err)
					}
					if err := in.RemoveViewMembers(id, ids[0]); err != nil {
						b.Fatal(err)
					}
					settleViews(in)
					ids[0], spare = spare, ids[0]
				}
			})
		}
	}
}

// 一个玩家挂接、解除一个关系视野：成本 = 成员数。
func BenchmarkRelationViewAttach(b *testing.B) {
	for _, members := range []int{32, 1024, 4096} {
		b.Run(fmt.Sprintf("members=%d", members), func(b *testing.B) {
			in := benchmarkInterest(b)
			const id = ViewID(1)
			if err := in.DefineRelationView(id, entity.SyncProfile{}); err != nil {
				b.Fatal(err)
			}
			ids := make([]int64, members)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			if err := in.SetViewMembers(id, ids); err != nil {
				b.Fatal(err)
			}
			settleViews(in)
			b.ReportAllocs()
			for b.Loop() {
				if err := in.AttachView(7, id); err != nil {
					b.Fatal(err)
				}
				settleViews(in)
				if err := in.DetachView(7, id); err != nil {
					b.Fatal(err)
				}
				settleViews(in)
			}
		})
	}
}

// 一个玩家多个空间视野（RTS 单位、阵营眼位）：每轮移动全部被跟随的单位，每个视野按自己的块重评。
func BenchmarkSpatialViewsPerPlayer(b *testing.B) {
	for _, views := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("views=%d", views), func(b *testing.B) {
			in := benchmarkInterest(b)
			for i := 0; i < 2000; i++ {
				if err := in.Show(int64(100_000+i), spatial.Point{X: int64(i%100)*100 + 50, Y: int64(i/100)*100 + 50}); err != nil {
					b.Fatal(err)
				}
			}
			for i := range views {
				unit := int64(1 + i)
				at := spatial.Point{X: int64(i%8)*1000 + 500, Y: int64(i/8)*500 + 500}
				if err := in.Show(unit, at); err != nil {
					b.Fatal(err)
				}
				if err := in.DefineSpatialView(ViewID(unit), at, ObserverView{}, unit); err != nil {
					b.Fatal(err)
				}
				if err := in.AttachView(7, ViewID(unit)); err != nil {
					b.Fatal(err)
				}
			}
			settleViews(in)
			b.ReportAllocs()
			step := int64(0)
			for b.Loop() {
				step = (step + 1) % 2
				for i := range views {
					unit := int64(1 + i)
					at := spatial.Point{X: int64(i%8)*1000 + 500 + step*100, Y: int64(i/8)*500 + 500}
					if err := in.Move(unit, at); err != nil {
						b.Fatal(err)
					}
				}
				settleViews(in)
			}
		})
	}
}

// 共享空间视野（阵营眼位挂给全阵营）：目标进出一个视野，扇出到全部挂接玩家。
func BenchmarkSharedSpatialView(b *testing.B) {
	for _, players := range []int{1, 64, 1024} {
		b.Run(fmt.Sprintf("players=%d", players), func(b *testing.B) {
			in := benchmarkInterest(b)
			if err := in.DefineSpatialView(1, spatial.Point{X: 500, Y: 500}, ObserverView{}, 0); err != nil {
				b.Fatal(err)
			}
			for player := 1; player <= players; player++ {
				if err := in.AttachView(int64(1_000_000+player), 1); err != nil {
					b.Fatal(err)
				}
			}
			if err := in.Show(42, spatial.Point{X: 510, Y: 500}); err != nil {
				b.Fatal(err)
			}
			settleViews(in)
			b.ReportAllocs()
			inside := true
			for b.Loop() {
				inside = !inside
				at := spatial.Point{X: 900, Y: 500}
				if inside {
					at = spatial.Point{X: 510, Y: 500}
				}
				if err := in.Move(42, at); err != nil {
					b.Fatal(err)
				}
				settleViews(in)
			}
		})
	}
}

// aoiWorld 是世界大小基准的一档：边界与块边长。视野参数取 demo 普通场景（scene.go：进入 120、离开 150、视野上限 300、
// 主视野 64），世界大小与块边长之外都不变。
type aoiWorld struct {
	name          string
	width, height int64
	blockSize     int64
}

// aoiWorlds：现有普通场景 1000×1000；开了地形时取地形图大小（厘米）——demo 缺省包 2×1 块（128m×64m）、块边长 100
// 下急切块表能容下的最大方图 16×16 块（1024m）、1w 方图（157×157 块，1,004,800 单位）。1w 在块边长 100 下超出
// spatial.MaxBlockCount（TestAOIWorldSizeBlockLimit），只能放大块边长：1000（约 2^20 块，上限内最小的整百）与地形块
// 边长 6400（与地形同步的 Interest 同一块）。
var aoiWorlds = []aoiWorld{
	{"1000x1000/block100", 1000, 1000, 100},
	{"terrain2x1/block100", 12_800, 6_400, 100},
	{"terrain16x16/block100", 102_400, 102_400, 100},
	{"terrain1w/block1000", 1_004_800, 1_004_800, 1000},
	{"terrain1w/block6400", 1_004_800, 1_004_800, 6400},
}

func (w aoiWorld) config() AOIConfig {
	return AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: w.width, Y: w.height}}, BlockSize: w.blockSize,
		EnterRadius: 120, LeaveRadius: 150, MinViewRadius: 60, MaxLeaveRadius: 300, ViewRadiusStep: 30, MaxVisible: 64,
	}
}

// populate 在世界中央放 512 个目标（32×16 网格，间距 spacing；最小的 1000×1000 也放得下，移动 80 不出界）与跟随前 128 个目标的观察者，与世界大小无关：
// 同样的人群放进不同的世界，比的是世界（块表）本身的成本。
func (w aoiWorld) populate(tb testing.TB, a *AOI, spacing int64) []spatial.Point {
	tb.Helper()
	origin := spatial.Point{X: w.width/2 - 16*spacing, Y: w.height/2 - 8*spacing}
	positions := make([]spatial.Point, 512)
	for i := range positions {
		positions[i] = spatial.Point{X: origin.X + int64(i%32)*spacing, Y: origin.Y + int64(i/32)*spacing}
		if err := a.AddSubject(int64(i+1), positions[i]); err != nil {
			tb.Fatal(err)
		}
	}
	for i := range 128 {
		if err := a.AddObserver(int64(i+1000), int64(i+1), positions[i], ObserverView{}); err != nil {
			tb.Fatal(err)
		}
	}
	a.Flush()
	a.FlushBlocks()
	return positions
}

// 块边长 100 时 1w 地图的块数超出急切块表的上限，构造被拒（不会悄悄分配上亿个块）；同一张图放大块边长后可以构造。
// 观察者的块预算按 MaxLeaveRadius / BlockSize 算、与世界大小无关（世界只会把它截小），所以每一档都在缺省预算内。
func TestAOIWorldSizeBlockLimit(t *testing.T) {
	big := aoiWorld{"terrain1w/block100", 1_004_800, 1_004_800, 100}
	if _, err := NewAOI(big.config()); !errors.Is(err, spatial.ErrTooManyBlocks) {
		t.Fatalf("1w map on 100-unit blocks: err = %v, want spatial.ErrTooManyBlocks", err)
	}
	for _, w := range aoiWorlds {
		if _, err := NewAOI(w.config()); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if worst := w.config().withViewDefaults().worstObserverBlocks(); worst > DefaultMaxObserverBlocks {
			t.Fatalf("%s: worst observer covers %d blocks, over the default budget %d", w.name, worst, DefaultMaxObserverBlocks)
		}
	}
}

// 世界大小对 AOI 的成本（地形场景取地形图大小之前的测量，见可编辑地形方案 §23）：
//   - new：构造（块表按块数急切分配，B/op 就是常驻内存）；blocks、observer-blocks 是块数与单个观察者最坏订阅块数；
//   - gc：AOI 常驻、人群已放入时一次完整 GC 的耗时（块表的指针要逐个标记）；
//   - move/spread|dense：同一群人（间距 25 / 10）每批 32 个目标与其中 8 个观察者移动 80，批量重评 + 收集事件。
func BenchmarkAOIWorldSize(b *testing.B) {
	for _, w := range aoiWorlds {
		b.Run("new/"+w.name, func(b *testing.B) {
			config := w.config()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := NewAOI(config); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(spatial.DivideCeil(w.width, w.blockSize)*spatial.DivideCeil(w.height, w.blockSize)), "blocks")
			b.ReportMetric(float64(config.withViewDefaults().worstObserverBlocks()), "observer-blocks")
		})
	}
	for _, w := range aoiWorlds {
		b.Run("gc/"+w.name, func(b *testing.B) {
			a, err := NewAOI(w.config())
			if err != nil {
				b.Fatal(err)
			}
			w.populate(b, a, 25)
			runtime.GC()
			for b.Loop() {
				runtime.GC()
			}
			runtime.KeepAlive(a)
		})
	}
	for _, w := range aoiWorlds {
		for _, spacing := range []int64{25, 10} {
			layout := "spread"
			if spacing == 10 {
				layout = "dense"
			}
			b.Run(fmt.Sprintf("move/%s/%s", layout, w.name), func(b *testing.B) {
				a, err := NewAOI(w.config())
				if err != nil {
					b.Fatal(err)
				}
				positions := w.populate(b, a, spacing)
				step := int64(0)
				b.ReportAllocs()
				for b.Loop() {
					a.beginBatch()
					for i := range 32 {
						at := positions[i]
						at.X += (step % 2) * 80
						if err := a.MoveSubject(int64(i+1), at); err != nil {
							b.Fatal(err)
						}
						if i%4 == 0 {
							if err := a.MoveObserver(int64(i+1000), at); err != nil {
								b.Fatal(err)
							}
						}
					}
					a.endBatch(1)
					a.Flush()
					a.FlushBlocks()
					step++
				}
			})
		}
	}
}

// byteSink 是只数字节的 Transport（基准用）。
type byteSink struct{ bytes atomic.Int64 }

func (s *byteSink) Push(_ context.Context, _ entitysync.SessionID, raw []byte) error {
	s.bytes.Add(int64(len(raw)))
	return nil
}

// 大联盟互见（KNOWN-LIMITS“AOI 视野的大联盟”）：players 个玩家同在一个共享关系视野里、互相可见（players² 对订阅），
// 经真实 Manager 的 Flush（ApplySync，政策阶段在 Flush 里）。进场全量先收敛（不计入）；之后每轮 1% 成员变化一次、
// Flush 一次。报告每轮耗时与字节、收敛后的堆（heap-MB，订阅表与视野登记的常驻内存）与订阅对数。
// 测量：go test ./framework/sync/entitysync/policy -run '^$' -bench RelationViewAllianceFlush -benchtime 50x -count 3。
func BenchmarkRelationViewAllianceFlush(b *testing.B) {
	for _, players := range allianceSizes {
		b.Run(fmt.Sprintf("players=%d", players), func(b *testing.B) {
			benchAllianceFlush(b, players)
		})
	}
}

func benchAllianceFlush(b *testing.B, players int) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	transport := &byteSink{}
	// 每个会话要容得下全体成员（缺省每会话 100 个对象）。
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: transport, Limits: frame.Limits{MaxObjects: players + 16}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = manager.Close(context.Background()) })
	in, err := NewInterest(InterestConfig{Manager: manager, ApplyMode: ApplySync, MaxPlayersPerView: 1 << 20, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 100_000, Y: 100_000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(in.Close)
	states := make([]*entity.SubjectSyncState, players)
	ids := make([]int64, players)
	for i := range players {
		id := int64(10_000 + i)
		ids[i] = id
		states[i] = subjectState(b, id)
		if err := manager.Register(states[i]); err != nil {
			b.Fatal(err)
		}
		if err := manager.OpenSession(entitysync.SessionID(id)); err != nil {
			b.Fatal(err)
		}
		// 彼此相距 300（离开半径 150）：空间视野里只有自己，可见全部来自联盟视野。
		if err := in.Enter(id, id, spatial.Point{X: int64(i%300)*300 + 150, Y: int64(i/300)*300 + 150}, ObserverView{}); err != nil {
			b.Fatal(err)
		}
	}
	const alliance = ViewID(1)
	if err := in.DefineRelationView(alliance, entity.SyncProfile{}); err != nil {
		b.Fatal(err)
	}
	if err := in.SetViewMembers(alliance, ids); err != nil {
		b.Fatal(err)
	}
	for _, id := range ids {
		if err := in.AttachView(id, alliance); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	// 进场全量：每会话每帧至多 MaxObjects 个对象，要若干次 Flush 才收敛；收敛判据是一次 Flush 不再发出字节。
	for round := 0; ; round++ {
		sent := transport.bytes.Load()
		if err := manager.Flush(ctx); err != nil {
			b.Fatal(err)
		}
		if round > 0 && transport.bytes.Load() == sent {
			break
		}
		if round > 10_000 {
			b.Fatal("the initial snapshots never converged")
		}
	}
	runtime.GC()
	var settled runtime.MemStats
	runtime.ReadMemStats(&settled)
	transport.bytes.Store(0)
	b.ReportAllocs()
	changed := max(players/100, 1)
	rounds, next := 0, 0
	for b.Loop() {
		for range changed {
			states[next].MarkDirty(1)
			next = (next + 1) % players
		}
		if err := manager.Flush(ctx); err != nil {
			b.Fatal(err)
		}
		rounds++
	}
	b.ReportMetric(float64(transport.bytes.Load())/float64(rounds), "bytes/round")
	b.ReportMetric(float64(int64(settled.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), "heap-MB")
	b.ReportMetric(float64(players)*float64(players), "pairs")
}

// allianceSizes 是 BenchmarkRelationViewAllianceFlush 的人数档；冒烟时临时调小。
var allianceSizes = []int{1000, 2000}
