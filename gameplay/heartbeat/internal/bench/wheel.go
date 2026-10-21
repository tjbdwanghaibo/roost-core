package bench

import "time"

// Wheel 是 D20 原方案的单层时间轮：分辨率 res（缺省 10ms），轮长 = 最长档 / res（10ms 时 6000 格）。
// 每格是切片、句柄记格号与下标，撤销 O(1)；每推进一格取出当前格全部条目，放回“当前格 + 周期格数”。
// 取空的格把切片还给空闲池、格本身置 nil，空格首次放入时从池里取：否则转过一圈后 6000 格都各自
// 留着一块按峰值扩容的数组（原型第一版的实测：5 万个 100ms 登记常驻约 5.9KB/登记）。
// 相位 = hash mod 轮长，再对周期格数取模，首拍规则与其他结构相同（firstFire）。
type Wheel struct {
	res   time.Duration
	slots [][]*WheelEntry
	pool  [][]*WheelEntry // 空闲切片（长度 0、保留容量）
	cur   int64
	size  int64
	count int
}

// WheelEntry 是 Wheel 的登记句柄。
type WheelEntry struct {
	id     int64
	hash   uint64
	period int32 // 周期格数
	slot   int32
	idx    int32
}

// NewWheel 构造单层时间轮。
func NewWheel(res time.Duration) *Wheel {
	size := framesOf(MaxPeriod, res)
	return &Wheel{res: res, slots: make([][]*WheelEntry, size), size: int64(size)}
}

func (w *Wheel) put(e *WheelEntry, at int64) {
	e.slot = int32(at % w.size)
	slot := &w.slots[e.slot]
	if *slot == nil && len(w.pool) > 0 {
		*slot = w.pool[len(w.pool)-1]
		w.pool = w.pool[:len(w.pool)-1]
	}
	e.idx = int32(len(*slot))
	*slot = append(*slot, e)
}

func (w *Wheel) remove(e *WheelEntry) {
	slot := &w.slots[e.slot]
	last := len(*slot) - 1
	moved := (*slot)[last]
	(*slot)[e.idx] = moved
	moved.idx = e.idx
	(*slot)[last] = nil
	*slot = (*slot)[:last]
	if last == 0 {
		w.pool = append(w.pool, *slot)
		*slot = nil
	}
}

func (w *Wheel) schedule(e *WheelEntry, tier Tier) {
	e.period = int32(framesOf(tier.Period(), w.res))
	phase := int(e.hash % uint64(w.size) % uint64(e.period))
	w.put(e, firstFire(w.cur, int(e.period), phase))
}

// Add 见 Scheduler。
func (w *Wheel) Add(id int64, hash uint64, tier Tier) *WheelEntry {
	e := &WheelEntry{id: id, hash: hash}
	w.schedule(e, tier)
	w.count++
	return e
}

// Remove 见 Scheduler。
func (w *Wheel) Remove(e *WheelEntry) {
	w.remove(e)
	w.count--
}

// SetTier 见 Scheduler。
func (w *Wheel) SetTier(e *WheelEntry, tier Tier) *WheelEntry {
	w.remove(e)
	w.schedule(e, tier)
	return e
}

// Step 见 Scheduler：当前格整体取出（格置 nil），逐项放回下一个触发格（周期 = 轮长时放回的是同一格，
// 从池里另取切片），取空的切片还给池。
func (w *Wheel) Step(out []int64) []int64 {
	w.cur++
	k := w.cur % w.size
	due := w.slots[k]
	if due == nil {
		return out
	}
	w.slots[k] = nil
	for _, e := range due {
		out = append(out, e.id)
		w.put(e, w.cur+int64(e.period))
	}
	clear(due)
	w.pool = append(w.pool, due[:0])
	return out
}

// Resolution 见 Scheduler。
func (w *Wheel) Resolution() time.Duration { return w.res }

// Len 见 Scheduler。
func (w *Wheel) Len() int { return w.count }
