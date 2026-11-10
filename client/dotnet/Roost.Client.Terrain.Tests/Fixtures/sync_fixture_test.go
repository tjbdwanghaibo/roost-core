//go:build terrainfixture

// TerrainChunk 同步夹具：由 regen.sh 复制进临时 module，与正式 DAO 生成器（codegen/cmd/dao）从 game-demo 的
// TerrainChunkDao 定义生成的代码同包编译。用生成 DAO 的 setter 在 Nest 隔离事务里改一块，取出 entitysync 会下发的
// 全量（MarshalSync）与增量（MarshalSyncChanges）字节，再用 gameplay/terrain 的 View 装入同一份覆盖块，记下若干位置
// 的期望查询结果。C# 测试（Roost.Client.Terrain.Tests）用生成的 C# Apply 合并这些字节、交给 TerrainView，逐项核对。
// 构建标签让仓库的 go build / vet ./... 跳过本文件。
package terraindao

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/terrain"
)

type fixtureQuery struct {
	X        int64  `json:"x"`
	Y        int64  `json:"y"`
	Height   int32  `json:"height"`
	Material byte   `json:"material"`
	Water    bool   `json:"water"`
	Error    string `json:"error,omitempty"`
}

type fixtureStep struct {
	Name           string         `json:"name"`
	Payload        string         `json:"payload"`
	Want           string         `json:"want"`
	Version        int64          `json:"version"`
	HeightsDelta   bool           `json:"heights_delta"`
	HeightsSent    bool           `json:"heights_sent"`
	MaterialsDelta bool           `json:"materials_delta"`
	MaterialsSent  bool           `json:"materials_sent"`
	Queries        []fixtureQuery `json:"queries"`
}

type fixture struct {
	Description string        `json:"description"`
	Pack        string        `json:"pack"`
	ChunkX      int32         `json:"chunk_x"`
	ChunkY      int32         `json:"chunk_y"`
	EntityID    int64         `json:"entity_id"`
	Steps       []fixtureStep `json:"steps"`
}

type fixtureCommitter struct{}

func (fixtureCommitter) Commit(_ context.Context, rec nest.CommitRecord) error {
	return dataengine.ValidateCommitRecord(rec)
}

// disk 返回块内以 (cx, cy) 为中心、半径 r 的圆覆盖的下标（裁到块内）。
func disk(cx, cy, r int) []int {
	var idx []int
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if x >= 0 && y >= 0 && x < terrain.ChunkSamples && y < terrain.ChunkSamples && (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				idx = append(idx, y*terrain.ChunkSamples+x)
			}
		}
	}
	return idx
}

func shift(v uint16, d int) uint16 { return uint16(min(max(int(v)+d, 0), 65535)) }

// widen / narrow 是 DAO 材质（[]uint16，为按下标增量而取的宽度）与 gameplay/terrain 每点 1 字节之间的换算
// （game-demo 在 terrain_chunk 组件边界做同样的事，值域 0～255）。
func widen(m []byte) []uint16 {
	out := make([]uint16, len(m))
	for i, v := range m {
		out[i] = uint16(v)
	}
	return out
}

func narrow(t *testing.T, m []uint16) []byte {
	out := make([]byte, len(m))
	for i, v := range m {
		if v > 255 {
			t.Fatalf("material %d at %d is outside a byte", v, i)
		}
		out[i] = byte(v)
	}
	return out
}

func TestWriteTerrainChunkSyncFixture(t *testing.T) {
	spec, out := os.Getenv("TERRAIN_SPEC"), os.Getenv("TERRAIN_FIXTURE_OUT")
	if spec == "" || out == "" {
		t.Fatal("TERRAIN_SPEC and TERRAIN_FIXTURE_OUT are required (run regen.sh)")
	}
	pack, err := terrain.OpenPack(filepath.Join(spec, "terrain-golden.pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer pack.Close()
	meta := pack.Meta()
	view := terrain.NewView(pack)
	for y := int32(0); y < meta.ChunksY; y++ {
		for x := int32(0); x < meta.ChunksX; x++ {
			if err := view.LoadBase(terrain.ChunkKey{X: x, Y: y}); err != nil {
				t.Fatal(err)
			}
		}
	}
	key := terrain.ChunkKey{X: 1, Y: 0}
	base, err := pack.ReadChunk(key)
	if err != nil {
		t.Fatal(err)
	}
	const entityID = int64(1)<<40 | 2

	d := NewTerrainChunkDao()
	d.SetId(entityID)
	d.baseRevision, d.version = meta.BaseRevision, 1
	d.heights = slices.Clone(base.Heights)
	for _, i := range disk(10, 10, 5) {
		d.heights[i] = shift(d.heights[i], 30)
	}
	d.materials = widen(base.Materials)
	d.Init()

	// 查询点：块 (1,0) 左边界两侧（块 (0,0) 右边一排的插值要读块 (1,0) 的第 0 列）、各次改动的范围、块内与右上角。
	var points [][2]int64
	for _, x := range []int64{6300, 6350, 6399, 6400, 6401, 6450, 7400, 7405, 9400, 9433, 12799, 12800, 12850} {
		for _, y := range []int64{0, 1000, 1500, 1999, 2000, 2050, 2550, 3000, 3033, 6399} {
			points = append(points, [2]int64{x, y})
		}
	}
	var mirror *SyncTerrainChunkDao
	var steps []fixtureStep
	record := func(name string, payload []byte) {
		want := d.MarshalSync(^uint64(0))
		patch, err := UnmarshalSyncTerrainChunkDao(payload)
		if err != nil {
			t.Fatal(err)
		}
		if mirror, err = ApplySyncTerrainChunkDao(patch, mirror); err != nil {
			t.Fatal(err)
		}
		if got, _ := MarshalSyncTerrainChunkDao(mirror); string(got) != string(want) {
			t.Fatalf("%s: Go apply did not reconstruct the DAO", name)
		}
		if err := view.SetOverlay(key, terrain.Overlay{BaseRevision: d.GetBaseRevision(), Version: d.GetVersion(),
			Heights: slices.Clone(d.heights), Materials: narrow(t, d.materials)}); err != nil {
			t.Fatal(err)
		}
		step := fixtureStep{Name: name, Payload: hex.EncodeToString(payload), Want: hex.EncodeToString(want), Version: d.GetVersion()}
		if patch.Heights != nil {
			step.HeightsSent, step.HeightsDelta = true, patch.Heights.Delta
		}
		if patch.Materials != nil {
			step.MaterialsSent, step.MaterialsDelta = true, patch.Materials.Delta
		}
		for _, p := range points {
			g, err := view.Probe(p[0], p[1])
			q := fixtureQuery{X: p[0], Y: p[1], Height: g.Height, Material: g.Material, Water: g.Water}
			if err != nil {
				q = fixtureQuery{X: p[0], Y: p[1], Error: "out_of_map"}
			}
			step.Queries = append(step.Queries, q)
		}
		steps = append(steps, step)
	}
	edit := func(name string, fn func()) {
		if _, err := nest.RunIsolatedTransaction(context.Background(), fixtureCommitter{}, "terrain", func() (any, error) { fn(); return nil, nil }); err != nil {
			t.Fatal(err)
		}
		mask := d.DirtyTracker().TakeEntitySyncDirty()
		captured := d.DirtyTracker().SyncChangeVersion()
		payload := d.MarshalSyncChanges(mask)
		d.DirtyTracker().AcknowledgeSyncChanges(captured)
		record(name, payload)
	}

	record("full", d.MarshalSync(^uint64(0)))
	// 小笔刷落在块的左边（第 0 列）：逐点 SetHeightsAt，同步是 list delta（按下标）。
	edit("edge_brush", func() {
		for _, i := range disk(0, 20, 5) {
			v, _ := d.GetHeights(i)
			d.SetHeightsAt(i, shift(v, -20))
		}
		d.SetVersion(d.GetVersion() + 1)
	})
	// 改材质（小范围）：逐点 SetMaterialsAt，同步是 list delta（按下标），高度不在补丁里（D11：材质也按下标增量）。
	edit("materials", func() {
		for _, i := range disk(30, 30, 8) {
			d.SetMaterialsAt(i, 9)
		}
		d.SetVersion(d.GetVersion() + 1)
	})
	// 改材质（大范围，整字段守卫）：SetMaterialsAll，同步是整字段替换；编号 ≥ 128 时 varint 两字节。
	edit("materials_whole", func() {
		m := slices.Clone(d.materials)
		for i := range terrain.ChunkPoints / 2 {
			m[i] = 200
		}
		d.SetMaterialsAll(m)
		d.SetVersion(d.GetVersion() + 1)
	})
	// 大改动（整字段守卫）：SetHeightsAll，同步是整字段替换。
	edit("whole_field", func() {
		h := slices.Clone(d.heights)
		for i := range h {
			h[i] = shift(h[i], 5)
		}
		d.SetHeightsAll(h)
		d.SetVersion(d.GetVersion() + 1)
	})
	shape := func(i int, heightsSent, heightsDelta, materialsSent, materialsDelta bool) {
		s := steps[i]
		if s.HeightsSent != heightsSent || s.HeightsDelta != heightsDelta || s.MaterialsSent != materialsSent || s.MaterialsDelta != materialsDelta {
			t.Fatalf("step %s: heights sent/delta %v/%v materials sent/delta %v/%v, want %v/%v %v/%v", s.Name,
				s.HeightsSent, s.HeightsDelta, s.MaterialsSent, s.MaterialsDelta, heightsSent, heightsDelta, materialsSent, materialsDelta)
		}
	}
	shape(1, true, true, false, false)  // edge_brush：高度按下标
	shape(2, false, false, true, true)  // materials：材质按下标
	shape(3, false, false, true, false) // materials_whole：材质整字段
	shape(4, true, false, false, false) // whole_field：高度整字段

	raw, err := json.MarshalIndent(fixture{
		Description: "TerrainChunk DAO sync payloads written by the generated Go DAO (codegen/cmd/dao on demo/db/def/terrain_chunk.go.tmpl) " +
			"and the gameplay/terrain View queries after each step. Regenerate with regen.sh.",
		Pack: "terrain-golden.pack", ChunkX: key.X, ChunkY: key.Y, EntityID: entityID, Steps: steps,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildingStep 是建筑同步夹具的一步：payload 是 entitysync 会下发的载荷（与 demo 建筑 packer 相同：全量 MarshalSync 带实体 ID，
// 增量 MarshalSyncDelta 按字段、不带实体 ID，客户端沿用基线的），
// want 是 Go 生成 DAO 此刻的全量；其余是客户端摆放建筑要读的字段（方案 §27：配置 ID + 参数 + 锚点 / 朝向 / 地基）。
type buildingStep struct {
	Name       string  `json:"name"`
	Payload    string  `json:"payload"`
	Want       string  `json:"want"`
	Full       bool    `json:"full"`
	State      int32   `json:"state"`
	Owner      int64   `json:"owner"`
	ConfigID   int32   `json:"config_id"`
	Params     []int32 `json:"params"`
	Facing     int32   `json:"facing"`
	GX         int32   `json:"gx"`
	GY         int32   `json:"gy"`
	Foundation int32   `json:"foundation"`
}

type buildingFixture struct {
	Description string         `json:"description"`
	EntityID    int64          `json:"entity_id"`
	Steps       []buildingStep `json:"steps"`
}

// TestWriteBuildingSyncFixture 写出 Building 同步夹具（方案 §27）：放置（全量）、改参数（只有 Params 的增量）、再放一种
// 没有参数的建筑（Params 清空）、拆除（State 0 与清空的字段）。C# 测试用生成的 Apply 合并，逐步与 Go 的全量相等。
func TestWriteBuildingSyncFixture(t *testing.T) {
	out := os.Getenv("BUILDING_FIXTURE_OUT")
	if out == "" {
		t.Fatal("BUILDING_FIXTURE_OUT is required (run regen.sh)")
	}
	const entityID = int64(13)<<40 | 6553711 // kind 13，锚点 (110, 100)
	d := NewBuildingDao()
	d.SetId(entityID)
	d.Init()
	var mirror *SyncBuildingDao
	var steps []buildingStep
	record := func(name string, payload []byte, full bool) {
		want := d.MarshalSync(^uint64(0))
		patch, err := UnmarshalSyncBuildingDao(payload)
		if err != nil {
			t.Fatal(err)
		}
		if full {
			mirror = nil
		}
		if mirror, err = ApplySyncBuildingDao(patch, mirror); err != nil {
			t.Fatal(err)
		}
		if got, _ := MarshalSyncBuildingDao(mirror); string(got) != string(want) {
			t.Fatalf("%s: Go apply did not reconstruct the DAO", name)
		}
		params := make([]int32, d.ParamsLen())
		for i := range params {
			params[i], _ = d.GetParams(i)
		}
		steps = append(steps, buildingStep{Name: name, Payload: hex.EncodeToString(payload), Want: hex.EncodeToString(want), Full: full,
			State: d.GetState(), Owner: d.GetOwner(), ConfigID: d.GetConfigID(), Params: params, Facing: d.GetFacing(),
			GX: d.GetGX(), GY: d.GetGY(), Foundation: d.GetFoundation()})
	}
	edit := func(name string, fn func()) []byte {
		if _, err := nest.RunIsolatedTransaction(context.Background(), fixtureCommitter{}, "building", func() (any, error) { fn(); return nil, nil }); err != nil {
			t.Fatal(err)
		}
		mask := d.DirtyTracker().TakeEntitySyncDirty()
		return d.MarshalSyncDelta(mask)
	}
	place := func(config int32, params []int32, facing int32, foundation int32) func() {
		return func() {
			d.SetState(1)
			d.SetOwner(4242)
			d.SetConfigID(config)
			d.SetParamsAll(params)
			d.SetFacing(facing)
			d.SetGX(110)
			d.SetGY(100)
			d.SetW(6)
			d.SetH(8)
			d.SetFoundation(foundation)
			d.SetBaseRevision(1)
		}
	}
	edit("place", place(2, []int32{3, 0}, 1, 50010))
	record("place_full", d.MarshalSync(^uint64(0)), true)
	record("params_delta", edit("params", func() { d.SetParamsAll([]int32{5, 9}) }), false)
	record("replace_without_params", edit("replace", place(4, nil, 0, -1<<31)), false)
	record("remove_delta", edit("remove", func() {
		d.SetState(0)
		d.SetOwner(0)
		d.SetConfigID(0)
		d.SetParamsAll(nil)
		d.SetFacing(0)
		d.SetGX(0)
		d.SetGY(0)
		d.SetW(0)
		d.SetH(0)
		d.SetFoundation(0)
		d.SetBaseRevision(0)
	}), false)
	if patch, _ := UnmarshalSyncBuildingDao(mustHex(t, steps[1].Payload)); patch.Params == nil || patch.ConfigID != nil || patch.State != nil {
		t.Fatalf("the parameter change carried more than the parameters: %+v", patch)
	}
	raw, err := json.MarshalIndent(buildingFixture{
		Description: "Building DAO sync payloads written by the generated Go DAO (codegen/cmd/dao on demo/db/def/building.go.tmpl). " +
			"Regenerate with regen.sh.",
		EntityID: entityID, Steps: steps,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
