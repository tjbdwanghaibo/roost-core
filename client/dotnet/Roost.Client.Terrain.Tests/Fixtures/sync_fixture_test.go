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
	Name         string         `json:"name"`
	Payload      string         `json:"payload"`
	Want         string         `json:"want"`
	Version      int64          `json:"version"`
	HeightsDelta bool           `json:"heights_delta"`
	HeightsSent  bool           `json:"heights_sent"`
	Queries      []fixtureQuery `json:"queries"`
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
	d.materials = slices.Clone(base.Materials)
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
			Heights: slices.Clone(d.heights), Materials: d.GetMaterialsAll()}); err != nil {
			t.Fatal(err)
		}
		step := fixtureStep{Name: name, Payload: hex.EncodeToString(payload), Want: hex.EncodeToString(want), Version: d.GetVersion()}
		if patch.Heights != nil {
			step.HeightsSent, step.HeightsDelta = true, patch.Heights.Delta
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
	// 改材质：整字段替换，高度不在补丁里。
	edit("materials", func() {
		m := d.GetMaterialsAll()
		for _, i := range disk(30, 30, 8) {
			m[i] = 9
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
	if !steps[1].HeightsDelta || steps[2].HeightsSent || steps[3].HeightsDelta || !steps[3].HeightsSent {
		t.Fatalf("unexpected patch shapes: %+v", steps)
	}

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
