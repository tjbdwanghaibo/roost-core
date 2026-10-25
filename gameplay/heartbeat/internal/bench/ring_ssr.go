package bench

import "time"

// SSRRing 按 ssr 全局心跳调度器的思路重写（只读参考 ssr modules/heartbeat/internal/auto_tick.go，未复制代码）：
//
//   - 基础帧 50ms；相位 = hash mod 1200（最长档 60s 的帧数），所有档共用；
//   - 4 个环，跨度 2 / 20 / 200 / 1200 帧；一个登记放进“跨度能被它的帧数整除”的第一个环，
//     插入该环里所有满足 i ≡ 相位 (mod 帧数) 的格（一个登记最多占 5 格）；
//   - 每格是一个 ID 集合（map），撤销要按同样的规则从每个命中的格里删除；
//   - 每帧把 4 个环当前格里的 ID 全部取出。
//
// 与 ssr 的差异：ssr 的格里放的是实体 ID（实体频率 = 最短登记档，同一实体只有一个调度项）；
// roost 同一实体的多个登记不合并，所以这里格里放的是登记 ID。ssr 每格预分配 map 容量 10000，MapHint 可设同样的值，
// 微基准缺省 0，并单独报告预分配 10000 时的固定内存。
type SSRRing struct {
	rings   [4]ssrRing
	frame   int64
	count   int
	mapHint int
}

type ssrRing struct {
	span  int
	cells []map[int64]struct{}
}

// SSRHandle 记录撤销所需的信息（ssr 的 agent 同样保存 id / tick / offset）。
type SSRHandle struct {
	id     int64
	frames int
	offset int
}

var ssrSpans = [4]int{2, 20, 200, 1200}

// ssrResolution 是 ssr 的基础帧。
const ssrResolution = 50 * time.Millisecond

// NewSSRRing 构造 ssr 式多环；mapHint 是每格 map 的预分配容量（ssr 生产值 10000）。
func NewSSRRing(mapHint int) *SSRRing {
	r := &SSRRing{mapHint: mapHint}
	for i, span := range ssrSpans {
		r.rings[i].span = span
		r.rings[i].cells = make([]map[int64]struct{}, span)
		for j := range r.rings[i].cells {
			r.rings[i].cells[j] = make(map[int64]struct{}, mapHint)
		}
	}
	return r
}

func (r *SSRRing) ringFor(frames int) *ssrRing {
	for i := range r.rings {
		if r.rings[i].span%frames == 0 {
			return &r.rings[i]
		}
	}
	panic("bench: tier does not fit any ssr ring")
}

// Add 见 Scheduler。
func (r *SSRRing) Add(id int64, hash uint64, tier Tier) SSRHandle {
	h := SSRHandle{id: id, frames: framesOf(tier.Period(), ssrResolution), offset: int(hash % uint64(framesOf(MaxPeriod, ssrResolution)))}
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		ring.cells[i][id] = struct{}{}
	}
	r.count++
	return h
}

// Remove 见 Scheduler。
func (r *SSRRing) Remove(h SSRHandle) {
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		delete(ring.cells[i], h.id)
	}
	r.count--
}

// SetTier 见 Scheduler：ssr 的换档就是撤销 + 重新登记。
func (r *SSRRing) SetTier(h SSRHandle, tier Tier) SSRHandle {
	r.Remove(h)
	h.frames = framesOf(tier.Period(), ssrResolution)
	ring := r.ringFor(h.frames)
	for i := h.offset % h.frames; i < ring.span; i += h.frames {
		ring.cells[i][h.id] = struct{}{}
	}
	r.count++
	return h
}

// Step 见 Scheduler。
func (r *SSRRing) Step(out []int64) []int64 {
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
func (r *SSRRing) Resolution() time.Duration { return ssrResolution }

// Len 见 Scheduler。
func (r *SSRRing) Len() int { return r.count }
