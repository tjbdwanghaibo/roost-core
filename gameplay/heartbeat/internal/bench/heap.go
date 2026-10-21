package bench

import "time"

// MinHeap 是按到期格号排序的二叉最小堆（分辨率 res，缺省 10ms，与时间轮相同以便对照）。
// 句柄记堆下标，撤销 / 改档 O(log n)；每推进一格反复取堆顶到期项，到期格加周期后原地下沉。
// 相位规则与时间轮相同（firstFire），所以同分辨率下两者的触发时刻逐格相同。
type MinHeap struct {
	res   time.Duration
	items []*HeapEntry
	cur   int64
	phase uint64
}

// HeapEntry 是 MinHeap 的登记句柄。
type HeapEntry struct {
	id     int64
	hash   uint64
	due    int64
	period int64
	idx    int32
}

// NewMinHeap 构造最小堆。
func NewMinHeap(res time.Duration) *MinHeap {
	return &MinHeap{res: res, phase: uint64(framesOf(MaxPeriod, res))}
}

func (h *MinHeap) less(i, j int) bool { return h.items[i].due < h.items[j].due }

func (h *MinHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].idx = int32(i)
	h.items[j].idx = int32(j)
}

func (h *MinHeap) up(j int) {
	for j > 0 {
		i := (j - 1) / 2
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *MinHeap) down(i int) bool {
	start, n := i, len(h.items)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		j := l
		if r := l + 1; r < n && h.less(r, l) {
			j = r
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > start
}

func (h *MinHeap) fix(i int) {
	if !h.down(i) {
		h.up(i)
	}
}

func (h *MinHeap) schedule(e *HeapEntry, tier Tier) {
	p := framesOf(tier.Period(), h.res)
	e.period = int64(p)
	e.due = firstFire(h.cur, p, int(e.hash%h.phase%uint64(p)))
}

// Add 见 Scheduler。
func (h *MinHeap) Add(id int64, hash uint64, tier Tier) *HeapEntry {
	e := &HeapEntry{id: id, hash: hash}
	h.schedule(e, tier)
	e.idx = int32(len(h.items))
	h.items = append(h.items, e)
	h.up(int(e.idx))
	return e
}

// Remove 见 Scheduler。
func (h *MinHeap) Remove(e *HeapEntry) {
	i, n := int(e.idx), len(h.items)-1
	if i != n {
		h.swap(i, n)
	}
	h.items[n] = nil
	h.items = h.items[:n]
	if i != n {
		h.fix(i)
	}
	e.idx = -1
}

// SetTier 见 Scheduler。
func (h *MinHeap) SetTier(e *HeapEntry, tier Tier) *HeapEntry {
	h.schedule(e, tier)
	h.fix(int(e.idx))
	return e
}

// Step 见 Scheduler。
func (h *MinHeap) Step(out []int64) []int64 {
	h.cur++
	for len(h.items) > 0 && h.items[0].due <= h.cur {
		e := h.items[0]
		out = append(out, e.id)
		e.due += e.period
		h.down(0)
	}
	return out
}

// Resolution 见 Scheduler。
func (h *MinHeap) Resolution() time.Duration { return h.res }

// Len 见 Scheduler。
func (h *MinHeap) Len() int { return len(h.items) }
