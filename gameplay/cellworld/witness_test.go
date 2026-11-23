package cellworld

import (
	"bytes"
	"cmp"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 旧路径独立建索引：AppendRadius 候选按 ID，先判圆，只有溢出才按饱和距离和有符号 ID 全排。
// 不复用生产 beatView 或选择函数，防止构建索引、重复 ID 或边界处理同时改错。
func referenceWitnesses(n *Node, r *Region) []outgoing {
	index, err := spatial.NewBlockIndex(n.indexBounds, n.cfg.IndexBlock)
	if err != nil {
		return nil
	}
	states := map[int64]EntityState{}
	var watchers []EntityState
	for id, l := range r.locals.m {
		if n.indexBounds.Contains(l.St.Pos()) {
			index.Add(id, l.St.Pos())
			states[id] = l.St
		}
		if l.Role == RoleReal && l.St.Kind == KindPlayer && l.St.Base != 0 {
			watchers = append(watchers, l.St)
		}
	}
	for id, g := range r.ghosts.m {
		if _, ok := states[id]; !ok && n.indexBounds.Contains(g.St.Pos()) {
			states[id] = g.St
			index.Add(id, g.St.Pos())
		}
	}
	slices.SortFunc(watchers, func(a, b EntityState) int {
		ax, ay, _ := n.refreshBlock(a.Pos())
		bx, by, _ := n.refreshBlock(b.Pos())
		if c := cmp.Compare(ay, by); c != 0 {
			return c
		}
		if c := cmp.Compare(ax, bx); c != 0 {
			if ay%2 != 0 {
				return -c
			}
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var out []outgoing
	for _, o := range watchers {
		ids := index.AppendRadius(nil, o.Pos(), n.cfg.ViewRadius)
		ids = slices.DeleteFunc(ids, func(id int64) bool { return !spatial.WithinDistance(o.Pos(), states[id].Pos(), n.cfg.ViewRadius) })
		truncated := len(ids) > n.cfg.MaxViewEntities
		if truncated {
			slices.SortFunc(ids, func(a, b int64) int {
				return cmp.Or(cmp.Compare(spatial.DistanceSquared(o.Pos(), states[a].Pos()), spatial.DistanceSquared(o.Pos(), states[b].Pos())), cmp.Compare(a, b))
			})
			ids = ids[:n.cfg.MaxViewEntities]
		}
		seen := make([]EntityState, len(ids))
		for i, id := range ids {
			seen[i] = states[id]
		}
		w := &witnessMsg{Observer: o.ID, Epoch: o.Ver.Epoch, Tick: r.tick, Self: o, Entities: seen, Truncated: truncated}
		out = append(out, outgoing{sid: o.Base, msg: &wireMsg{Kind: kindWitness, Witness: w}})
	}
	return out
}

func checkWitnesses(t *testing.T, n *Node, r *Region) {
	t.Helper()
	want := referenceWitnesses(n, r)
	var got []outgoing
	n.witnesses(r, &beatView{}, &got)
	if len(got) != len(want) {
		t.Fatalf("got %d witnesses, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].sid != want[i].sid || !reflect.DeepEqual(got[i].msg, want[i].msg) {
			t.Fatalf("witness %d: got sid %d %+v; want sid %d %+v", i, got[i].sid, got[i].msg.Witness, want[i].sid, want[i].msg.Witness)
		}
		g, e := encodeMsg(got[i].msg)
		if e != nil {
			t.Fatal(e)
		}
		w, e := encodeMsg(want[i].msg)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("witness %d: wire mismatch", i)
		}
	}
}

func TestWitnessSelectionReference(t *testing.T) {
	p := func(x, y int64) spatial.Point { return spatial.Point{X: x, Y: y} }
	rnd := rand.New(rand.NewPCG(19, 71))
	for trial := 0; trial < 64; trial++ {
		n := &Node{cfg: NodeConfig{ViewRadius: 1 + rnd.Int64N(24), MaxViewEntities: 1 + rnd.IntN(20), IndexBlock: 1 + rnd.Int64N(20)}, indexBounds: spatial.Rect{Min: p(-44, -39), Max: p(42, 38)}}
		r := NewRegion()
		r.tick = uint64(trial + 1)
		for i := int64(-63); i <= 64; i++ {
			st := EntityState{ID: i, Kind: KindNPC, X: rnd.Int64N(96) - 48, Y: rnd.Int64N(96) - 48, HP: int32(i + 80), Ver: Version{Epoch: 1, Seq: uint64(i + 64)}}
			if i%8 == 0 {
				st.Kind, st.Base = KindPlayer, int32(1+rnd.IntN(3))
			}
			if i%3 != 0 {
				r.locals.m[i] = localRec{Role: RoleReal, St: st}
			}
			if i%5 == 0 || i%3 == 0 {
				st.X, st.Y, st.HP = rnd.Int64N(80)-40, rnd.Int64N(80)-40, 999
				r.ghosts.m[i] = ghostRec{St: st}
			}
		}
		checkWitnesses(t, n, r)
	}
	// 同中心、同距、圆周、块的半开上边、极端有符号 ID；含重复 Ghost、被冻结的玩家与 ID=0（索引拒绝）。
	r := NewRegion()
	r.tick = 23
	self := EntityState{ID: 9, Kind: KindPlayer, Base: 7, X: 0, Y: 0, Ver: Version{Epoch: 2, Seq: 7}}
	r.locals.m[9] = localRec{Role: RoleReal, St: self}
	for id, at := range map[int64]spatial.Point{math.MinInt64: p(0, 0), -9: p(-5, 0), -2: p(3, 4), 0: p(0, 0), 2: p(0, 5), 3: p(5, 0), 10: p(0, -5), math.MaxInt64: p(0, 0), 77: p(4, 4)} {
		r.ghosts.m[id] = ghostRec{St: EntityState{ID: id, Kind: KindNPC, X: at.X, Y: at.Y, HP: 42}}
	}
	r.ghosts.m[self.ID] = ghostRec{St: EntityState{ID: self.ID, HP: -3}}
	r.locals.m[-9] = localRec{Role: RoleFrozen, St: EntityState{ID: -9, Kind: KindPlayer, Base: 1, X: -5, HP: 41}}
	for _, block := range []int64{5, 7} {
		for k := 1; k <= 12; k++ {
			checkWitnesses(t, &Node{cfg: NodeConfig{ViewRadius: 5, IndexBlock: block, MaxViewEntities: k}, indexBounds: spatial.Rect{Min: p(-10, -10), Max: p(11, 11)}}, r)
		}
	}
	// 距离平方超过 MaxInt64 时，排序不是按高精度真距离（都饱和后按 ID）。加减半径也可能溢出。
	for _, origin := range []int64{math.MinInt64, math.MaxInt64 - 12_000_000_000} {
		r := NewRegion()
		self.X, self.Y = origin+1, origin+1
		r.locals.m[9] = localRec{Role: RoleReal, St: self}
		for id, dx := range map[int64]int64{-100: 5_000_000_000, 2: 4_000_000_000, 7: 3_900_000_000, 4: 9_100_000_000} {
			r.ghosts.m[id] = ghostRec{St: EntityState{ID: id, X: self.X + dx, Y: self.Y}}
		}
		n := &Node{cfg: NodeConfig{ViewRadius: 9_000_000_000, IndexBlock: 3_000_000_000, MaxViewEntities: 2}, indexBounds: spatial.Rect{Min: p(origin, origin), Max: p(origin+12_000_000_000, origin+12_000_000_000)}}
		for k := 1; k <= 6; k++ {
			n.cfg.MaxViewEntities = k
			checkWitnesses(t, n, r)
		}
		self.X = origin + 11_999_999_999
		r.locals.m[9] = localRec{Role: RoleReal, St: self}
		checkWitnesses(t, n, r)
	}
	n := &Node{cfg: NodeConfig{ViewRadius: 4, IndexBlock: 3, MaxViewEntities: 1}, indexBounds: spatial.Rect{Min: p(20, 20), Max: p(30, 30)}}
	checkWitnesses(t, n, NewRegion())
	checkWitnesses(t, n, r)
}

// 从 measureLevel 的 2×2 布局与随机种子构造 (1,0) 的静态初始快照，每轮计时包括全新 beatView 和完整输出。
// 与多进程窗口分开：不测移动、收发、复制编码、调度；不能据此推断窗口 p99。
func BenchmarkWitnessBeatScales(b *testing.B) {
	for _, name := range []string{"game", "demo"} {
		b.Run(name, func(b *testing.B) {
			s := measureScales[name]
			g, err := (childSpec{Cols: 2, Rows: 2, CellSize: s.cellSize, Band: s.band, Hysteresis: s.hysteresis}).grid()
			if err != nil {
				b.Fatal(err)
			}
			cell := CellID{1, 0}
			n := &Node{cfg: NodeConfig{ViewRadius: s.viewRadius, IndexBlock: s.viewRadius, MaxViewEntities: s.maxView}, indexBounds: expand(g.Rect(cell), s.band)}
			r := NewRegion()
			watchers := 0
			add := func(st EntityState) {
				owner, _ := g.CellAt(st.Pos())
				if owner == cell {
					r.locals.m[st.ID] = localRec{Role: RoleReal, St: st}
					if st.Kind == KindPlayer {
						watchers++
					}
				} else if n.indexBounds.Contains(st.Pos()) {
					r.ghosts.m[st.ID] = ghostRec{Owner: owner, St: st}
				}
			}
			for i, owner := range g.Cells() {
				rect := g.Rect(owner)
				rnd := rand.New(rand.NewPCG(42, uint64(owner.Col)<<32|uint64(owner.Row)))
				for j := range 2250 {
					_ = rnd.Float64() // 实际 Spawn 会先抽一个速度方向，再抽坐标。
					add(EntityState{ID: npcID(1_000_000*int64(i+1) + int64(j)), Kind: KindNPC, X: rect.Min.X + rnd.Int64N(s.cellSize), Y: rect.Min.Y + rnd.Int64N(s.cellSize), HP: 100, Ver: Version{Epoch: 1, Seq: 1}})
				}
			}
			rnd := rand.New(rand.NewPCG(7, 7))
			for id := int64(1); id <= 1000; id++ {
				add(EntityState{ID: actorID(id), Kind: KindPlayer, Base: 1, X: 1000 + rnd.Int64N(2*s.cellSize-2000), Y: 1000 + rnd.Int64N(2*s.cellSize-2000), HP: 100, Ver: Version{Epoch: 1, Seq: 1}})
			}
			b.ReportAllocs()
			for b.Loop() {
				var out []outgoing
				n.witnesses(r, &beatView{}, &out)
				if len(out) != watchers {
					b.Fatalf("%d witnesses, want %d", len(out), watchers)
				}
			}
			b.ReportMetric(float64(watchers), "observers/op")
			b.ReportMetric(float64(len(r.ghosts.m)), "ghosts/op")
		})
	}
}
