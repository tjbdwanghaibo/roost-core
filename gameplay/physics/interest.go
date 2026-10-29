package physics

import (
	"cmp"
	"errors"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
)

// SpatialFrame 是一次已提交物理帧的不可变发布视图。
// 空间索引每帧建一次，连接只读；AOI Block 与线协议 Chunk 可以使用不同尺寸。
//
// 索引是两张与对象数等长的切片，不用每帧的散列表：positions 与 Snapshot.Objects 同下标；
// blocks 按 (Block, 对象下标) 排序，查询时对每个 Block 二分找到起点，同一 Block 内仍按对象下标次序，
// 与逐个追加进 Block 的输出次序相同。建好后不再修改。
type SpatialFrame struct {
	Snapshot  chunksync.Snapshot
	positions [][3]float64
	blocks    []blockEntry
	ordered   bool // 对象 ID 严格递增（物理房间的原生输出），观察者可二分查找
	blockSize float64
}

type blockEntry struct {
	key   [2]int64
	index int
}

func NewSpatialFrame(f Frame, chunkSize, blockSize float32) (*SpatialFrame, error) {
	if blockSize < 1 || math.IsNaN(float64(blockSize)) || math.IsInf(float64(blockSize), 0) {
		return nil, errors.New("physics: invalid AOI block size")
	}
	snapshot, err := Snapshot(f, chunkSize)
	if err != nil {
		return nil, err
	}
	return spatialFrame(f, snapshot, blockSize)
}

// NewQuantizedSpatialFrame 为物理房间构建量化网络视图；AOI 仍以原始权威坐标筛选。
func NewQuantizedSpatialFrame(f Frame, blockSize float32) (*SpatialFrame, error) {
	if blockSize < 1 || math.IsNaN(float64(blockSize)) || math.IsInf(float64(blockSize), 0) {
		return nil, errors.New("physics: invalid AOI block size")
	}
	snapshot, err := QuantizedSnapshot(f)
	if err != nil {
		return nil, err
	}
	return spatialFrame(f, snapshot, blockSize)
}
func spatialFrame(f Frame, snapshot chunksync.Snapshot, blockSize float32) (*SpatialFrame, error) {
	s := &SpatialFrame{Snapshot: snapshot, positions: make([][3]float64, len(f.States)), blocks: make([]blockEntry, len(f.States)), ordered: true, blockSize: float64(blockSize)}
	for i, state := range f.States {
		p := [3]float64{float64(state.Position[0]), float64(state.Position[2]), float64(state.Position[1])}
		if math.Abs(p[0]/s.blockSize) > math.MaxInt32 || math.Abs(p[1]/s.blockSize) > math.MaxInt32 {
			return nil, errors.New("physics: AOI coordinate out of range")
		}
		s.positions[i] = p
		s.blocks[i] = blockEntry{key: s.block(p[0], p[1]), index: i}
		if i > 0 && f.States[i-1].ID >= state.ID {
			s.ordered = false
		}
	}
	// 稳定排序保持同一 Block 内的对象下标次序。
	slices.SortStableFunc(s.blocks, func(a, b blockEntry) int { return compareBlock(a.key, b.key) })
	return s, nil
}
func compareBlock(a, b [2]int64) int {
	if c := cmp.Compare(a[0], b[0]); c != 0 {
		return c
	}
	return cmp.Compare(a[1], b[1])
}

// observer 返回观察者对象的下标；同一 ID 出现多次时取最后一个（与按 ID 覆盖写入一致）。
func (s *SpatialFrame) observer(id uint64) (int, bool) {
	objects := s.Snapshot.Objects
	if s.ordered {
		return slices.BinarySearchFunc(objects, id, func(o chunksync.Object, id uint64) int { return cmp.Compare(o.ID, id) })
	}
	for i := len(objects) - 1; i >= 0; i-- {
		if objects[i].ID == id {
			return i, true
		}
	}
	return 0, false
}
func (s *SpatialFrame) block(x, z float64) [2]int64 {
	return [2]int64{int64(math.Floor(x / s.blockSize)), int64(math.Floor(z / s.blockSize))}
}

// Visible 根据当前权威位置选择对象。previous 必须来自上次成功发送的视野，不能提前结算。
// 新对象进入 radius，已可见对象保留到 radius+margin；进入发完整对象，离开发 remove 由 Cursor 负责。
// XZ Block 粗筛后按 XYZ 中心距离裁剪，不作遮挡判定；自己始终可见，不让客户端指定观察坐标。
func (s *SpatialFrame) Visible(observer uint64, radius, margin float64, previous map[uint64]bool) (chunksync.Snapshot, map[uint64]bool, error) {
	return s.VisibleInto(nil, nil, observer, radius, margin, previous)
}

// VisibleInto 与 Visible 相同，但复用调用方交回的存储（nil 时新建）：
//   - objects 截为 0 后作为返回快照的对象表，调用方在下一次复用前必须已用完上一次结果；
//   - selected 先清空再写入新可见集合，必须是已不再使用的旧视野（典型是 ACK 后换下的那份），不能是 previous。
func (s *SpatialFrame) VisibleInto(objects []chunksync.Object, selected map[uint64]bool, observer uint64, radius, margin float64, previous map[uint64]bool) (chunksync.Snapshot, map[uint64]bool, error) {
	if radius <= 0 || margin < 0 || math.IsNaN(radius+margin) || math.IsInf(radius+margin, 0) || radius+margin > 1000 {
		return chunksync.Snapshot{}, nil, errors.New("physics: invalid AOI radius")
	}
	at, ok := s.observer(observer)
	if !ok {
		return chunksync.Snapshot{}, nil, errors.New("physics: missing observer")
	}
	center := s.positions[at]
	outer := radius + margin
	first, last := s.block(center[0]-outer, center[1]-outer), s.block(center[0]+outer, center[1]+outer)
	// 防止不合理的半径/Block 配置触发巨量空格扫描，单次最多访问 4096 格。
	if (last[0]-first[0]+1)*(last[1]-first[1]+1) > 4096 {
		return chunksync.Snapshot{}, nil, errors.New("physics: AOI query exceeds block budget")
	}
	// 稳定视野下本次可见数接近上次，按 previous 预留容量，避免逐个追加时反复扩容。
	if objects == nil {
		objects = make([]chunksync.Object, 0, len(previous))
	}
	out := chunksync.Snapshot{Epoch: s.Snapshot.Epoch, Tick: s.Snapshot.Tick, Objects: objects[:0]}
	if selected == nil {
		selected = make(map[uint64]bool, len(previous))
	} else {
		clear(selected)
	}
	for x := first[0]; x <= last[0]; x++ {
		for z := first[1]; z <= last[1]; z++ {
			key := [2]int64{x, z}
			first, _ := slices.BinarySearchFunc(s.blocks, key, func(e blockEntry, key [2]int64) int { return compareBlock(e.key, key) })
			for _, e := range s.blocks[first:] {
				if e.key != key {
					break
				}
				i := e.index
				object := s.Snapshot.Objects[i]
				p := s.positions[i]
				dx, dz, dy := p[0]-center[0], p[1]-center[1], p[2]-center[2]
				limit := radius
				if previous[object.ID] {
					limit = outer
				}
				if object.ID == observer || dx*dx+dz*dz+dy*dy <= limit*limit {
					out.Objects = append(out.Objects, object)
					selected[object.ID] = true
				}
			}
		}
	}
	return out, selected, nil
}
