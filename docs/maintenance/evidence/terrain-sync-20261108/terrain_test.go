//go:build terrainexp

// 地形块 DAO 形状实验（方案稿 D8）：A = Heights []byte（2 字节/点小端，整字段替换），
// B = Heights []uint16（list delta）。全部离线：提交器是内存替身，不连数据库、
// 不经过 Nest 队列与网络。run.sh 把本文件复制进临时 module，与正式生成器的产物同包编译。
package terrain

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	side   = 64
	points = side * side
	chunk  = int64(7)
)

// baseHeights 生成一块平滑的合成地形：10cm 精度下约 350m～650m（3500～6500），
// 加 ±2 的抖动。offset 用于把数值整体抬到 16384 以上，观察 varint 宽度的影响。
func baseHeights(offset int) []uint16 {
	rng := rand.New(rand.NewPCG(20261108, 64))
	h := make([]uint16, points)
	for y := range side {
		for x := range side {
			fx, fy := float64(x), float64(y)
			v := 5000 + 900*math.Sin(fx/23+0.3)*math.Cos(fy/17) + 300*math.Sin((fx+fy)/7) + 60*math.Sin(fx/3.1)*math.Sin(fy/2.7)
			v += float64(rng.IntN(5)) - 2 + float64(offset)
			h[y*side+x] = uint16(v)
		}
	}
	return h
}

func baseMaterials() []byte {
	m := make([]byte, points)
	for i := range m {
		m[i] = byte((i/side)/16*4 + (i%side)/16) // 16 个 16×16 的材质片
	}
	return m
}

func encodeLE(h []uint16) []byte {
	out := make([]byte, 2*len(h))
	for i, v := range h {
		binary.LittleEndian.PutUint16(out[2*i:], v)
	}
	return out
}

func decodeLE(raw []byte) []uint16 {
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return out
}

// disk 返回以 (cx, cy) 为中心、半径 r 的圆形笔刷覆盖的块内下标（裁到块内）。
func disk(cx, cy, r int) []int {
	var idx []int
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if x < 0 || y < 0 || x >= side || y >= side {
				continue
			}
			if dx, dy := x-cx, y-cy; dx*dx+dy*dy <= r*r {
				idx = append(idx, y*side+x)
			}
		}
	}
	return idx
}

func allPoints() []int {
	idx := make([]int, points)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

func newListDao(h []uint16) *TerrainListDao {
	d := NewTerrainListDao()
	d.SetId(chunk)
	d.baseRevision, d.version = 1, 1
	d.heights = slices.Clone(h)
	d.materials = baseMaterials()
	d.Init()
	d.DirtyTracker().SetVersion(1) // 已持久过一次：后续提交走 Patch，而不是首次 Put。
	return d
}

func newBytesDao(heights []byte) *TerrainBytesDao {
	d := NewTerrainBytesDao()
	d.SetId(chunk)
	d.baseRevision, d.version = 1, 1
	d.heights = slices.Clone(heights)
	d.materials = baseMaterials()
	d.Init()
	d.DirtyTracker().SetVersion(1)
	return d
}

type recordCommitter struct{ records []nest.CommitRecord }

func (c *recordCommitter) Commit(_ context.Context, rec nest.CommitRecord) error {
	if err := dataengine.ValidateCommitRecord(rec); err != nil {
		return err
	}
	c.records = append(c.records, rec)
	return nil
}

// commit 在一个 Nest 隔离事务里执行业务修改，返回该事务冻结的唯一 Mutation。
func commit(t testing.TB, fn func()) dataengine.Mutation {
	t.Helper()
	c := &recordCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), c, "terrain", func() (any, error) { fn(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if len(c.records) != 1 || len(c.records[0].Mutations) != 1 {
		t.Fatalf("records: %+v", c.records)
	}
	return c.records[0].Mutations[0]
}

// 业务笔刷（B）：逐点调用生成 setter，版本加一。
func brushList(d *TerrainListDao, idx []int, delta int) {
	for _, i := range idx {
		v, _ := d.GetHeights(i)
		d.SetHeightsAt(i, uint16(int(v)+delta))
	}
	d.SetVersion(d.GetVersion() + 1)
}

// 业务笔刷（A，未压缩）：取整段副本、改动、整段写回，版本加一。
func brushBytes(d *TerrainBytesDao, idx []int, delta int) {
	raw := d.GetHeightsAll()
	for _, i := range idx {
		v := binary.LittleEndian.Uint16(raw[2*i:])
		binary.LittleEndian.PutUint16(raw[2*i:], uint16(int(v)+delta))
	}
	d.SetHeightsAll(raw)
	d.SetVersion(d.GetVersion() + 1)
}

func deflate(t testing.TB, raw []byte, level int) []byte {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func inflate(t testing.TB, raw []byte) []byte {
	out, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rowDelta 是常见的业务层预处理：每行与上一点做差（zigzag），平滑地形差值小，压缩更好。
func rowDelta(h []uint16) []byte {
	out := make([]byte, 0, 2*len(h))
	for y := range side {
		prev := 0
		for x := range side {
			v := int(h[y*side+x])
			d := v - prev
			prev = v
			out = binary.AppendUvarint(out, uint64((d<<1)^(d>>63)))
		}
	}
	return out
}

// persistSummary 描述一次提交冻结的 BSON 更新：$set 键数、字节数、heights 是整字段还是按下标。
func persistSummary(m dataengine.Mutation) (keys int, size int, heights string) {
	if m.Kind != dataengine.MutationPatch {
		return 0, len(m.Data), "Put(" + fmt.Sprint(m.Kind) + ")"
	}
	elements, _ := bson.Raw(m.Patch.SetBSON).Elements()
	heights = "无"
	perIndex := 0
	for _, e := range elements {
		switch {
		case e.Key() == "3":
			heights = "整字段"
		case strings.HasPrefix(e.Key(), "3."):
			perIndex++
		}
	}
	if perIndex > 0 {
		heights = fmt.Sprintf("按下标 %d 个", perIndex)
	}
	return len(elements), len(m.Patch.SetBSON), heights
}

type report struct{ b strings.Builder }

func (r *report) line(format string, args ...any) { fmt.Fprintf(&r.b, format+"\n", args...) }
func (r *report) flush(t *testing.T, name string) {
	t.Log("\n" + r.b.String())
	if dir := os.Getenv("TERRAIN_OUT"); dir != "" {
		if err := os.WriteFile(dir+"/"+name, []byte(r.b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	maskSyncA = terrainBytesDaoFieldHeights | terrainBytesDaoFieldVersion
	maskSyncB = terrainListDaoFieldHeights | terrainListDaoFieldVersion
)

// checkClientApply 断言客户端用生成的 Go Apply 把 B 的补丁应用到上一份全量后，
// 与服务端当前的 4096 点完全一致（PB 视图与镜像 DAO 两条路径）。
func checkClientApply(t *testing.T, baseline, patch []byte, d *TerrainListDao) {
	t.Helper()
	previous, err := UnmarshalSyncTerrainListDao(baseline)
	if err != nil {
		t.Fatal(err)
	}
	p, err := UnmarshalSyncTerrainListDao(patch)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ApplySyncTerrainListDao(p, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(next.Heights.Value, d.heights) || next.Version.Value != d.version {
		t.Fatal("client view differs from server heights")
	}
	mirror := NewTerrainListDao()
	if err := mirror.ApplySync(baseline); err != nil {
		t.Fatal(err)
	}
	if err := mirror.ApplySync(patch); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mirror.heights, d.heights) || !bytes.Equal(mirror.MarshalSync(^uint64(0)), d.MarshalSync(^uint64(0))) {
		t.Fatal("mirror DAO differs from server")
	}
}

func isDelta(t *testing.T, raw []byte) bool {
	t.Helper()
	p, err := UnmarshalSyncTerrainListDao(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p.Heights != nil && p.Heights.Delta
}

// 测量 1 + 3 + 5：单次编辑的同步字节、持久化更新与客户端应用。
func TestTerrainSingleEdit(t *testing.T) {
	base := baseHeights(0)
	var r report
	r.line("| 编辑 | 改动点数 | A 同步（Heights+Version） | A flate 后同步 | B 同步（Heights+Version） | B 形式 | A 持久 $set | B 持久 $set | B 持久 heights 形式 |")
	r.line("| --- | ---: | ---: | ---: | ---: | --- | ---: | ---: | --- |")
	type tc struct {
		name string
		idx  []int
	}
	var cases []tc
	for _, radius := range []int{1, 2, 5, 10, 20, 32} {
		cases = append(cases, tc{fmt.Sprintf("r=%dm", radius), disk(32, 32, radius)})
	}
	cases = append(cases, tc{"全块 4096 点都改（逐点 SetHeightsAt，+5）", allPoints()})
	for _, c := range cases {
		// A 未压缩
		a := newBytesDao(encodeLE(base))
		ma := commit(t, func() { brushBytes(a, c.idx, 5) })
		aSync := a.MarshalSyncChanges(maskSyncA)
		if !bytes.Equal(aSync, a.MarshalSync(maskSyncA)) {
			t.Fatal("A changes differ from full field encoding")
		}
		_, aPersist, _ := persistSummary(ma)
		// A 压缩后存：DAO 里放 flate(LE)，同步的就是压缩字节。
		edited := decodeLE(a.heights)
		ac := newBytesDao(deflate(t, encodeLE(base), flate.DefaultCompression))
		commit(t, func() {
			ac.SetHeightsAll(deflate(t, encodeLE(edited), flate.DefaultCompression))
			ac.SetVersion(ac.GetVersion() + 1)
		})
		acSync := ac.MarshalSync(maskSyncA)
		if got := decodeLE(inflate(t, ac.heights)); !slices.Equal(got, edited) {
			t.Fatal("compressed round trip")
		}
		// B
		b := newListDao(base)
		baseline := b.MarshalSync(^uint64(0))
		mb := commit(t, func() { brushList(b, c.idx, 5) })
		bSync := b.MarshalSyncChanges(maskSyncB)
		if !slices.Equal(b.heights, edited) {
			t.Fatal("A and B diverged")
		}
		checkClientApply(t, baseline, bSync, b)
		form := "delta"
		if !isDelta(t, bSync) {
			form = "整字段回退"
		}
		_, bPersist, bForm := persistSummary(mb)
		r.line("| %s | %d | %d | %d | %d | %s | %d | %d | %s |", c.name, len(c.idx), len(aSync), len(acSync), len(bSync), form, aPersist, bPersist, bForm)
	}
	// 整块整平的另一种写法：B 用 SetHeightsAll 整体替换。
	b := newListDao(base)
	flat := slices.Repeat([]uint16{5000}, points)
	mb := commit(t, func() { b.SetHeightsAll(flat); b.SetVersion(2) })
	bSync := b.MarshalSyncChanges(maskSyncB)
	_, bPersist, bForm := persistSummary(mb)
	a := newBytesDao(encodeLE(base))
	ma := commit(t, func() { a.SetHeightsAll(encodeLE(flat)); a.SetVersion(2) })
	_, aPersist, _ := persistSummary(ma)
	ac := newBytesDao(deflate(t, encodeLE(base), flate.DefaultCompression))
	commit(t, func() { ac.SetHeightsAll(deflate(t, encodeLE(flat), flate.DefaultCompression)); ac.SetVersion(2) })
	r.line("| 整块整平（SetHeightsAll，平面） | 4096 | %d | %d | %d | %s | %d | %d | %s |", len(a.MarshalSync(maskSyncA)), len(ac.MarshalSync(maskSyncA)), len(bSync), map[bool]string{true: "delta", false: "整字段"}[isDelta(t, bSync)], aPersist, bPersist, bForm)
	r.flush(t, "single-edit.md")
}

// 测量 1 的阈值：逐点改前 k 个不同下标，看同步与持久化在哪个 k 回退为整字段。
func TestTerrainFallbackThreshold(t *testing.T) {
	base := baseHeights(0)
	var r report
	r.line("| 改动不同下标数 k | Version 写入顺序 | B 同步形式 | B 同步字节 | B 持久 heights 形式 | B 持久 $set 字节 |")
	r.line("| ---: | --- | --- | ---: | --- | ---: |")
	firstFull := map[string]int{}
	for _, versionFirst := range []bool{false, true} {
		order := "之后"
		if versionFirst {
			order = "之前"
		}
		for k := 4085; k <= 4096; k++ {
			b := newListDao(base)
			baseline := b.MarshalSync(^uint64(0))
			idx := allPoints()[:k]
			m := commit(t, func() {
				if versionFirst {
					b.SetVersion(2)
				}
				for _, i := range idx {
					v, _ := b.GetHeights(i)
					b.SetHeightsAt(i, v+1)
				}
				if !versionFirst {
					b.SetVersion(2)
				}
			})
			raw := b.MarshalSyncChanges(maskSyncB)
			checkClientApply(t, baseline, raw, b)
			form := "delta"
			if !isDelta(t, raw) {
				form = "整字段"
				if _, ok := firstFull["sync/"+order]; !ok {
					firstFull["sync/"+order] = k
				}
			}
			_, size, pform := persistSummary(m)
			if pform == "整字段" {
				if _, ok := firstFull["persist/"+order]; !ok {
					firstFull["persist/"+order] = k
				}
			}
			r.line("| %d | %s | %s | %d | %s | %d |", k, order, form, len(raw), pform, size)
		}
	}
	r.line("")
	r.line("首次回退：同步（Version 之后写）k=%d，同步（Version 之前写）k=%d；持久化（之后）k=%d，持久化（之前）k=%d。",
		firstFull["sync/之后"], firstFull["sync/之前"], firstFull["persist/之后"], firstFull["persist/之前"])
	// 同一下标反复写不新增节点：4000 个下标各写 3 次仍是 delta。
	b := newListDao(base)
	commit(t, func() {
		for range 3 {
			for i := range 4000 {
				v, _ := b.GetHeights(i)
				b.SetHeightsAt(i, v+1)
			}
		}
	})
	r.line("同一事务内 4000 个下标各写 3 次：B 同步 delta=%t。", isDelta(t, b.MarshalSyncChanges(maskSyncB)))
	r.flush(t, "fallback-threshold.md")
}

// 测量 2：同一块在一次同步前被多次编辑（每次编辑是独立事务，同步变化树跨事务累积直到确认）。
func TestTerrainMultiEditBeforeSync(t *testing.T) {
	base := baseHeights(0)
	var r report
	r.line("| 场景 | 编辑次数 | 累计不同下标 | B 同步字节 | B 形式 | A 同步字节（任意次数都一样） | 每次编辑 B 持久 $set 字节（逐次） |")
	r.line("| --- | ---: | ---: | ---: | --- | ---: | --- |")
	type tc struct {
		name    string
		centers [][2]int
		radius  int
	}
	for _, c := range []tc{
		{"5 × r=5 不重叠", [][2]int{{8, 8}, {24, 8}, {40, 8}, {56, 8}, {8, 24}}, 5},
		{"5 × r=5 重叠（中心相距 1m）", [][2]int{{30, 32}, {31, 32}, {32, 32}, {33, 32}, {34, 32}}, 5},
		{"5 × r=5 同一中心", [][2]int{{32, 32}, {32, 32}, {32, 32}, {32, 32}, {32, 32}}, 5},
		{"4 × r=20 四角（大面积）", [][2]int{{16, 16}, {48, 16}, {16, 48}, {48, 48}}, 20},
		{"5 × r=20 四角 + 中心", [][2]int{{16, 16}, {48, 16}, {16, 48}, {48, 48}, {32, 32}}, 20},
	} {
		b := newListDao(base)
		a := newBytesDao(encodeLE(base))
		baseline := b.MarshalSync(^uint64(0))
		unique := map[int]bool{}
		var persists []string
		for n, center := range c.centers {
			idx := disk(center[0], center[1], c.radius)
			for _, i := range idx {
				unique[i] = true
			}
			delta := 3
			if n%2 == 1 {
				delta = -1
			}
			m := commit(t, func() { brushList(b, idx, delta) })
			commit(t, func() { brushBytes(a, idx, delta) })
			_, size, _ := persistSummary(m)
			persists = append(persists, fmt.Sprint(size))
		}
		raw := b.MarshalSyncChanges(maskSyncB)
		checkClientApply(t, baseline, raw, b)
		if !slices.Equal(b.heights, decodeLE(a.heights)) {
			t.Fatal("A and B diverged")
		}
		form := "delta"
		if !isDelta(t, raw) {
			form = "整字段回退"
		}
		r.line("| %s | %d | %d | %d | %s | %d | %s |", c.name, len(c.centers), len(unique), len(raw), form, len(a.MarshalSync(maskSyncA)), strings.Join(persists, " / "))
	}
	// 连续编辑 + 每次确认：客户端顺序应用 50 个补丁后与服务端一致。
	b := newListDao(base)
	view, _ := UnmarshalSyncTerrainListDao(b.MarshalSync(^uint64(0)))
	rng := rand.New(rand.NewPCG(7, 11))
	for range 50 {
		idx := disk(rng.IntN(side), rng.IntN(side), 1+rng.IntN(12))
		commit(t, func() { brushList(b, idx, rng.IntN(7)-3) })
		captured := b.DirtyTracker().SyncChangeVersion()
		p, err := UnmarshalSyncTerrainListDao(b.MarshalSyncChanges(maskSyncB))
		if err != nil {
			t.Fatal(err)
		}
		b.DirtyTracker().AcknowledgeSyncChanges(captured)
		if view, err = ApplySyncTerrainListDao(p, view); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(view.Heights.Value, b.heights) {
		t.Fatal("sequential client view diverged")
	}
	r.line("")
	r.line("顺序应用：50 次随机笔刷（r=1～12，每次确认后取下一份补丁），客户端视图与服务端 4096 点一致。")
	r.flush(t, "multi-edit.md")
}

// 测量 3：整文档 BSON 大小与压缩大小、全量同步大小（含 varint 宽度敏感性）。
func TestTerrainDocumentSizes(t *testing.T) {
	var r report
	r.line("| 数据 | A 整文档 BSON | A flate 整文档 BSON | B 整文档 BSON | A 全量同步 | A flate 全量同步 | B 全量同步 | Heights 原始 | flate(LE) | flate(行差分 varint) |")
	r.line("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, v := range []struct {
		name   string
		offset int
	}{{"高度 3500～6500（varint 2 字节）", 0}, {"高度 +16384 以上（varint 3 字节）", 16384}} {
		h := baseHeights(v.offset)
		le := encodeLE(h)
		a := newBytesDao(le)
		ac := newBytesDao(deflate(t, le, flate.DefaultCompression))
		b := newListDao(h)
		r.line("| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d |", v.name, len(a.Marshal()), len(ac.Marshal()), len(b.Marshal()),
			len(a.MarshalSync(^uint64(0))), len(ac.MarshalSync(^uint64(0))), len(b.MarshalSync(^uint64(0))),
			len(le), len(deflate(t, le, flate.DefaultCompression)), len(deflate(t, rowDelta(h), flate.DefaultCompression)))
	}
	r.line("")
	r.line("全量同步与整文档都含 Materials（4096 字节，两种形状相同）、BaseRevision、Version。")
	r.flush(t, "document-sizes.md")
}
