package policy

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
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

// ---- 视野（D19）：只测登记表与应用侧扇出（syncViewsLocked / drainAOILocked 写进 held），不含 Manager 订阅与打包 ----

func benchmarkInterest(b *testing.B) *Interest {
	b.Helper()
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = manager.Close(context.Background()) })
	in, err := NewInterest(InterestConfig{Manager: manager, MaxPlayersPerView: 1 << 20, AOI: AOIConfig{
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
	in.syncViewsLocked()
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
