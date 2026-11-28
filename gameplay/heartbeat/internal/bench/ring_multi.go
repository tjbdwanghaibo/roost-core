package bench

import "time"

// MultiRing 按既有项目全局心跳调度器的思路重写（只读参考既有实现，未复制代码）：
//
//   - 基础帧 50ms；相位 = hash mod 1200（最长档 60s 的帧数），所有档共用；
//   - 4 个环，跨度 2 / 20 / 200 / 1200 帧；一个登记放进“跨度能被它的帧数整除”的第一个环，
//     插入该环里所有满足 i ≡ 相位 (mod 帧数) 的格（一个登记最多占 5 格）；
//   - 每格是一个 ID 集合（map），撤销要按同样的规则从每个命中的格里删除；
//   - 每帧把 4 个环当前格里的 ID 全部取出。
//
// 与既有项目的差异：既有项目的格里放的是实体 ID（实体频率 = 最短登记档，同一实体只有一个调度项）；
// roost 同一实体的多个登记不合并，所以这里格里放的是登记 ID。既有项目每格预分配 map 容量 10000，MapHint 可设同样的值，
// 微基准缺省 0，并单独报告预分配 10000 时的固定内存。
type MultiRing struct {
	rings   [4]multiRing
	frame   int64
	count   int
	mapHint int
}

type multiRing struct {
	span  int
	cells []map[int64]struct{}
}

// MultiRingHandle 记录撤销所需的信息。
type MultiRingHandle struct {
	id     int64
	frames int
	offset int
}

var multiRingSpans = [4]int{2, 20, 200, 1200}

// multiRingResolution 是既有项目的基础帧。
const multiRingResolution = 50 * time.Millisecond

// NewMultiRing 构造既有项目式多环；mapHint 是每格 map 的预分配容量（既有项目生产值 10000）。
func NewMultiRing(mapHint int) *MultiRing {
	r := &MultiRing{mapHint: mapHint}
	for i, span := range multiRingSpans {
		r.rings[i].span = span
		r.rings[i].cells = make([]map[int64]struct{}, span)
		for j := range r.rings[i].cells {
			r.rings[i].cells[j] = make(map[int64]struct{}, mapHint)
		}
	}
	return r
}

func (r *MultiRing) ringFor(frames int) *multiRing {
	for i := range r.rings {
		if r.rings[i].span%frames == 0 {
			return &r.rings[i]
		}
	}
	panic("bench: tier does not fit any multi ring")
}

// Add 见 Scheduler。
func (r *MultiRing) Add(id int64, hash uint64, tier Tier) MultiRingHandle {
	h := MultiRingHandle{id: id, frames: framesOf(tier.Period(), multiRingResolution), offset: int(hash % uint64(framesOf(MaxPeriod, multiRingResolution)))}
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		ring.cells[i][id] = struct{}{}
	}
	r.count++
	return h
}

// Remove 见 Scheduler。
func (r *MultiRing) Remove(h MultiRingHandle) {
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		delete(ring.cells[i], h.id)
	}
	r.count--
}

// SetTier 见 Scheduler：既有项目的换档就是撤销 + 重新登记。
func (r *MultiRing) SetTier(h MultiRingHandle, tier Tier) MultiRingHandle {
	r.Remove(h)
	h.frames = framesOf(tier.Period(), multiRingResolution)
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		ring.cells[i][h.id] = struct{}{}
	}
	r.count++
	return h
}

// Step 见 Scheduler。
func (r *MultiRing) Step(out []int64) []int64 {
	r.frame++
	for i := range r.rings {
		ring := &r.rings[i]
		for id := range ring.cells[r.frame%int64(ring.span)] {
			out = append(out, id)
		}
	}
	return out
}

// Resolution 见 Scheduler。
func (r *MultiRing) Resolution() time.Duration { return multiRingResolution }

// Len 见 Scheduler。
func (r *MultiRing) Len() int { return r.count }
