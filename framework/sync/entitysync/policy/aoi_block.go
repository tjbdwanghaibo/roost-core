package policy

import (
	"cmp"
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// aoiBlock 只存索引引用。位置真相仍由 subject 唯一持有；物理成员索引复用
// spatial.BlockIndex 的块锁。登记与查询被批次写/读阶段隔开，不在块锁内计算可见性。
type aoiBlock struct {
	mu        sync.RWMutex
	observers map[int64]*interestObserver
	observed  map[int64]struct{}
}

func (m *AOI) block(id int64) *aoiBlock {
	if b := m.blocks[id]; b != nil {
		return b
	}
	b := &aoiBlock{observers: make(map[int64]*interestObserver), observed: make(map[int64]struct{})}
	m.blocks[id] = b
	return b
}

// BlockAt 返回此 AOI 中位置对应的块编号。编号仅在本 AOI 内有效，越界为-1。
func (m *AOI) BlockAt(at spatial.Point) int64 { return m.index.BlockIndex(at) }

func (m *AOI) checkedBlocks(blocks []int64) ([]int64, error) {
	next := slices.Clone(blocks)
	slices.Sort(next)
	next = slices.Compact(next)
	if len(next) > m.config.MaxObservedBlocks {
		return nil, fmt.Errorf("%w: observed blocks %d exceed %d", ErrInterestBudget, len(next), m.config.MaxObservedBlocks)
	}
	for _, id := range next {
		if m.index.BlockRect(id).Empty() {
			return nil, spatial.ErrInvalidBounds
		}
	}
	return next, nil
}

// SetObservedBlocks 替换实体的额外被观察块；空集合清除覆盖，不改变真实位置。
// 调用方拥有 AOI 的写阶段。正式业务通过 Interest.QueueObservedBlocks 走提交门。
func (m *AOI) SetObservedBlocks(id int64, blocks []int64) error {
	if _, ok := m.subjects[id]; !ok {
		return ErrInterestUnknown
	}
	next, err := m.checkedBlocks(blocks)
	if err != nil {
		return err
	}
	previous := m.observed[id]
	affected := make(map[int64]*interestObserver)
	// 按有序差集更新，未改变的覆盖无需重写。读阶段要等整个写批次结束。
	for _, blockID := range previous {
		if _, keep := slices.BinarySearch(next, blockID); keep {
			continue
		}
		b := m.block(blockID)
		b.mu.Lock()
		delete(b.observed, id)
		for oid, o := range b.observers {
			affected[oid] = o
		}
		b.mu.Unlock()
	}
	for _, blockID := range next {
		if _, keep := slices.BinarySearch(previous, blockID); keep {
			continue
		}
		b := m.block(blockID)
		b.mu.Lock()
		b.observed[id] = struct{}{}
		for oid, o := range b.observers {
			affected[oid] = o
		}
		b.mu.Unlock()
	}
	if len(next) == 0 {
		delete(m.observed, id)
	} else {
		m.observed[id] = next
	}
	for _, o := range affected {
		if m.batching {
			m.dirty[o.id] = o
		} else {
			m.evaluateObserved(o)
		}
	}
	return nil
}

func (m *AOI) evaluateObserved(o *interestObserver) {
	next := make(map[int64]int)
	for blockID := range o.blocks {
		if b := m.blocks[blockID]; b != nil {
			b.mu.RLock()
			for id := range b.observed {
				if id != o.id {
					next[id] = 0
				}
			}
			b.mu.RUnlock()
		}
	}
	for id := range o.blockVisible {
		if _, ok := next[id]; !ok {
			o.blockPending = append(o.blockPending, InterestEvent{Observer: o.id, Subject: id, Kind: InterestLeave, Band: -1})
		}
	}
	for id := range next {
		if _, ok := o.blockVisible[id]; !ok {
			o.blockPending = append(o.blockPending, InterestEvent{Observer: o.id, Subject: id, Kind: InterestEnter})
		}
	}
	o.blockVisible = next
}

type blockSource struct{ manager *AOI }

func (blockSource) Name() string { return SourceBlock }
func (s blockSource) Flush() []InterestEvent {
	m := s.manager
	events := m.blockPending
	m.blockPending = nil
	for _, o := range m.observers {
		events = append(events, o.blockPending...)
		o.blockPending = nil
	}
	sortInterestEvents(events)
	return events
}

func sortInterestEvents(events []InterestEvent) {
	slices.SortStableFunc(events, func(a, b InterestEvent) int {
		if n := cmp.Compare(a.Observer, b.Observer); n != 0 {
			return n
		}
		return cmp.Compare(a.Subject, b.Subject)
	})
}

// beginBatch/endBatch 是一个政策批次的写/读屏障。写入阶段由协调者串行
// 执行，随后不同 observer 并行计算；同一 observer 不会并发写自己的可见集。
// 不在确认线程、Nest worker 或每个 block 上创建常驻处理器。
func (m *AOI) beginBatch() { m.batching = true }
func (m *AOI) endBatch(workers int) {
	m.batching = false
	observers := make([]*interestObserver, 0, len(m.dirty))
	for _, o := range m.dirty {
		observers = append(observers, o)
	}
	clear(m.dirty)
	slices.SortFunc(observers, func(a, b *interestObserver) int { return cmp.Compare(a.id, b.id) })
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(observers))
	if workers <= 1 {
		for _, o := range observers {
			m.evaluateObserver(o)
		}
		return
	}
	var wg sync.WaitGroup
	// 每批最多 Workers 个短任务，无第二层排队；任务结束后才允许下一批写入。
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := worker; i < len(observers); i += workers {
				m.evaluateObserver(observers[i])
			}
		})
	}
	wg.Wait()
}
