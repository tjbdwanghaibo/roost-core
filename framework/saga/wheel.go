package saga

import "time"

// wheel 是分区循环内的哈希时间轮（设计 §4.7，从原型移植）：只在分区循环的 goroutine 里用，不加锁。槽宽 tick，按到期时刻
// 落槽，每次推进扫过去的槽，只取出已到期的条目，没到期的（多转一圈的）留在槽里。条目只是“到时看一眼”的提示：状态机按记录
// 当前的 NextRunAt 判定（planDue 之前先核对），陈旧条目不改状态，所以重排时不必删除旧条目。
type wheel struct {
	tick   int64 // 槽宽（纳秒）
	slots  [][]wheelEntry
	cursor int64 // 下一个要扫的 tick（绝对 tick 号 = unix 纳秒 / tick）
	size   int
}

type wheelEntry struct {
	id string
	at int64 // unix 纳秒
}

func newWheel(tick time.Duration, slots int, now time.Time) *wheel {
	if tick <= 0 {
		tick = 5 * time.Millisecond
	}
	if slots <= 0 {
		slots = 1024
	}
	return &wheel{tick: int64(tick), slots: make([][]wheelEntry, slots), cursor: now.UnixNano() / int64(tick)}
}

// schedule 登记 id 在 at 到期。已经过去的时刻落在下一次推进会扫的槽里。
func (w *wheel) schedule(id string, at time.Time) {
	nanos := at.UnixNano()
	tick := nanos / w.tick
	if tick < w.cursor {
		tick = w.cursor
	}
	slot := int(tick % int64(len(w.slots)))
	w.slots[slot] = append(w.slots[slot], wheelEntry{id: id, at: nanos})
	w.size++
}

// advance 扫到 now 为止的全部 tick，把到期条目的 saga ID 追加到 due（可能重复）后返回。已过去的 tick 扫完就前进；now 所在的
// tick 只扫不前进：同一 tick 里还没到期的条目下次再看，不能推迟一整圈（原型的轮缺陷，已修）。落后超过一圈时每个槽扫一遍即可。
func (w *wheel) advance(now time.Time, due []string) []string {
	nanos := now.UnixNano()
	target := nanos / w.tick
	if target < w.cursor {
		// 时钟回拨（墙钟被向后调）：这期间登记的条目都被夹在 cursor 槽里，按各自的到期时刻判定；别的槽等时间追上来再扫。
		return w.scan(int(w.cursor%int64(len(w.slots))), nanos, due)
	}
	if target-w.cursor >= int64(len(w.slots)) {
		w.cursor = target - int64(len(w.slots)) + 1
	}
	for ; w.cursor < target; w.cursor++ {
		due = w.scan(int(w.cursor%int64(len(w.slots))), nanos, due)
	}
	return w.scan(int(target%int64(len(w.slots))), nanos, due)
}

func (w *wheel) scan(slot int, now int64, due []string) []string {
	entries := w.slots[slot]
	kept := entries[:0]
	for _, e := range entries {
		if e.at <= now {
			due = append(due, e.id)
			w.size--
		} else {
			kept = append(kept, e)
		}
	}
	clear(entries[len(kept):])
	w.slots[slot] = kept
	return due
}

// pending 是轮里还没取出的条目数（含陈旧条目）。
func (w *wheel) pending() int { return w.size }
