package jslogproto

// wheel 是分区循环内的哈希时间轮（单 goroutine 使用，不加锁）：槽宽 tickMs，按到期时刻落槽，每 tick 扫一个槽，
// 只取出已到期的条目，没到期的（多转一圈的）留在槽里。条目只是“到时看一眼”的提示：状态机按 saga 当前的
// DeadlineAt / NextRunAt 判定，陈旧条目不改状态，所以重排不必删除旧条目。
type wheel struct {
	tickMs int64
	slots  [][]wheelEntry
	cursor int64 // 下一个要扫的 tick（绝对 tick 号）
}

type wheelEntry struct {
	id string
	at int64
}

func newWheel(tickMs int64, slots int, now int64) *wheel {
	return &wheel{tickMs: tickMs, slots: make([][]wheelEntry, slots), cursor: now / tickMs}
}

func (w *wheel) schedule(id string, at int64) {
	tick := at / w.tickMs
	if tick < w.cursor {
		tick = w.cursor
	}
	slot := int(tick % int64(len(w.slots)))
	w.slots[slot] = append(w.slots[slot], wheelEntry{id: id, at: at})
}

// advance 扫到 now 为止的全部 tick，返回到期条目的 saga ID（可能重复）。已过去的 tick 扫完就前进；now 所在的 tick
// 只扫不前进（同一 tick 里还没到期的条目下次再看，不能等一整圈）。落后超过一圈时每个槽都扫一遍即可。
func (w *wheel) advance(now int64, due []string) []string {
	target := now / w.tickMs
	if target-w.cursor >= int64(len(w.slots)) {
		w.cursor = target - int64(len(w.slots)) + 1
	}
	for ; w.cursor < target; w.cursor++ {
		due = w.scan(int(w.cursor%int64(len(w.slots))), now, due)
	}
	return w.scan(int(target%int64(len(w.slots))), now, due)
}

func (w *wheel) scan(slot int, now int64, due []string) []string {
	entries := w.slots[slot]
	kept := entries[:0]
	for _, e := range entries {
		if e.at <= now {
			due = append(due, e.id)
		} else {
			kept = append(kept, e)
		}
	}
	for i := len(kept); i < len(entries); i++ {
		entries[i] = wheelEntry{}
	}
	w.slots[slot] = kept
	return due
}

func (w *wheel) size() int {
	n := 0
	for _, s := range w.slots {
		n += len(s)
	}
	return n
}
