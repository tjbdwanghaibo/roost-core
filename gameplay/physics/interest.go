package physics

import (
	"errors"
	"math"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
)

// SpatialFrame 是一次已提交物理帧的不可变发布视图。
// 空间索引每帧建一次，连接只读；AOI Block 与线协议 Chunk 可以使用不同尺寸。
type SpatialFrame struct {
	Snapshot  chunksync.Snapshot
	positions map[uint64][3]float64
	blocks    map[[2]int64][]int
	blockSize float64
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
	s := &SpatialFrame{Snapshot: snapshot, positions: make(map[uint64][3]float64, len(f.States)), blocks: make(map[[2]int64][]int), blockSize: float64(blockSize)}
	for i, state := range f.States {
		p := [3]float64{float64(state.Position[0]), float64(state.Position[2]), float64(state.Position[1])}
		if math.Abs(p[0]/s.blockSize) > math.MaxInt32 || math.Abs(p[1]/s.blockSize) > math.MaxInt32 {
			return nil, errors.New("physics: AOI coordinate out of range")
		}
		s.positions[state.ID] = p
		key := s.block(p[0], p[1])
		s.blocks[key] = append(s.blocks[key], i)
	}
	return s, nil
}
func (s *SpatialFrame) block(x, z float64) [2]int64 {
	return [2]int64{int64(math.Floor(x / s.blockSize)), int64(math.Floor(z / s.blockSize))}
}

// Visible 根据当前权威位置选择对象。previous 必须来自上次成功发送的视野，不能提前结算。
// 新对象进入 radius，已可见对象保留到 radius+margin；进入发完整对象，离开发 remove 由 Cursor 负责。
// XZ Block 粗筛后按 XYZ 中心距离裁剪，不作遮挡判定；自己始终可见，不让客户端指定观察坐标。
func (s *SpatialFrame) Visible(observer uint64, radius, margin float64, previous map[uint64]bool) (chunksync.Snapshot, map[uint64]bool, error) {
	if radius <= 0 || margin < 0 || math.IsNaN(radius+margin) || math.IsInf(radius+margin, 0) || radius+margin > 1000 {
		return chunksync.Snapshot{}, nil, errors.New("physics: invalid AOI radius")
	}
	center, ok := s.positions[observer]
	if !ok {
		return chunksync.Snapshot{}, nil, errors.New("physics: missing observer")
	}
	outer := radius + margin
	first, last := s.block(center[0]-outer, center[1]-outer), s.block(center[0]+outer, center[1]+outer)
	// 防止不合理的半径/Block 配置触发巨量空格扫描，单次最多访问 4096 格。
	if (last[0]-first[0]+1)*(last[1]-first[1]+1) > 4096 {
		return chunksync.Snapshot{}, nil, errors.New("physics: AOI query exceeds block budget")
	}
	// 稳定视野下本次可见数接近上次，按 previous 预留容量，避免逐个追加时反复扩容。
	out := chunksync.Snapshot{Epoch: s.Snapshot.Epoch, Tick: s.Snapshot.Tick, Objects: make([]chunksync.Object, 0, len(previous))}
	selected := make(map[uint64]bool, len(previous))
	for x := first[0]; x <= last[0]; x++ {
		for z := first[1]; z <= last[1]; z++ {
			for _, i := range s.blocks[[2]int64{x, z}] {
				object := s.Snapshot.Objects[i]
				p := s.positions[object.ID]
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
