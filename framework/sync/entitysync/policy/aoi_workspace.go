package policy

// observerDirty 在同一批次中合并原因；距离变化不改变额外块覆盖关系。
// dirtySpatial：观察者自己的位置、视野或订阅块变了，完整重评；dirtyMoved：只有附近的目标动了，
// 移动的目标记在 interestObserver.moved，能证明与完整重评结果相同时只评估这些目标（见 evaluateMoved）。
type observerDirty uint8

const (
	dirtySpatial observerDirty = 1 << iota
	dirtyCoverage
	dirtyMoved
)

// aoiWorkspace 只保存计算中的 ID 和值，不保存 observer 或对外返回的事件。
// 串行阶段独占 serialWork；每个并行 worker 独占自己的工作区，屏障后才回收。
type aoiWorkspace struct {
	visible, blocks, query []int64
	candidates             []entryCandidate
	seen                   map[int64]struct{}
	seenPeak               int
	// fullScans 计本工作区做过的完整候选遍历次数，屏障后由 collectScans 归入 AOI.fullScans。
	fullScans int
}

const aoiWorkspaceLimit = 4096
const aoiCachedWorkers = 32

func (w *aoiWorkspace) release() {
	if cap(w.visible) > aoiWorkspaceLimit {
		w.visible = nil
	} else {
		w.visible = w.visible[:0]
	}
	if cap(w.blocks) > aoiWorkspaceLimit {
		w.blocks = nil
	} else {
		w.blocks = w.blocks[:0]
	}
	if cap(w.query) > aoiWorkspaceLimit {
		w.query = nil
	} else {
		w.query = w.query[:0]
	}
	if cap(w.candidates) > aoiWorkspaceLimit {
		w.candidates = nil
	} else {
		w.candidates = w.candidates[:0]
	}
	if w.seenPeak > aoiWorkspaceLimit {
		w.seen = nil
	} else {
		clear(w.seen)
	}
	w.seenPeak = 0
}

func (m *AOI) refreshObserver(o *interestObserver, reason observerDirty) {
	if m.batching {
		o.dirty |= reason
		m.dirty[o.id] = o
		return
	}
	m.evaluateObserver(o, &m.serialWork, reason)
	m.collectScans(&m.serialWork)
	m.serialWork.release()
}

// collectScans 把工作区的完整遍历计数并入 AOI（只在串行阶段或并行屏障之后调用）。
func (m *AOI) collectScans(w *aoiWorkspace) {
	m.fullScans += uint64(w.fullScans)
	w.fullScans = 0
}
