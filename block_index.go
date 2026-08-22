package spatial

import (
	"sort"
	"sync"
)

// MaxBlockCount bounds eager lock allocation and rejects configurations that
// would otherwise cause excessive memory use during room creation.
const MaxBlockCount int64 = 1 << 20

type indexBlock struct {
	mu      sync.RWMutex
	ids     map[int64]struct{}
	version uint64
}

// BlockIndex is a concurrent ID-only spatial index. It owns partitioning and
// version tracking; application object lookup and visibility policy stay in
// the caller.
type BlockIndex struct {
	bounds    Rect
	blockSize int64
	cols      int64
	rows      int64
	blocks    []*indexBlock
}

func NewBlockIndex(bounds Rect, blockSize int64) (*BlockIndex, error) {
	bounds = NormalizeRect(bounds)
	if bounds.Empty() || blockSize <= 0 {
		return nil, ErrInvalidBounds
	}
	width, widthOK := safeSpan(bounds.Min.X, bounds.Max.X)
	height, heightOK := safeSpan(bounds.Min.Y, bounds.Max.Y)
	if !widthOK || !heightOK {
		return nil, ErrInvalidBounds
	}
	cols, rows := divideCeil(width, blockSize), divideCeil(height, blockSize)
	if cols <= 0 || rows <= 0 || cols > MaxBlockCount/rows {
		return nil, ErrTooManyBlocks
	}
	blockCount := cols * rows
	index := &BlockIndex{bounds: bounds, blockSize: blockSize, cols: cols, rows: rows, blocks: make([]*indexBlock, blockCount)}
	for offset := range index.blocks {
		index.blocks[offset] = &indexBlock{}
	}
	return index, nil
}

func (i *BlockIndex) Bounds() Rect {
	if i == nil {
		return Rect{}
	}
	return i.bounds
}

func (i *BlockIndex) BlockIndex(point Point) int64 {
	if i == nil || i.blockSize <= 0 || !i.bounds.Contains(point) {
		return -1
	}
	x := (point.X - i.bounds.Min.X) / i.blockSize
	y := (point.Y - i.bounds.Min.Y) / i.blockSize
	if x < 0 || x >= i.cols || y < 0 || y >= i.rows {
		return -1
	}
	return y*i.cols + x
}

func (i *BlockIndex) BlockRect(index int64) Rect {
	if i == nil || index < 0 || index >= int64(len(i.blocks)) {
		return Rect{}
	}
	x, y := index%i.cols, index/i.cols
	rect := Rect{
		Min: Point{X: i.bounds.Min.X + x*i.blockSize, Y: i.bounds.Min.Y + y*i.blockSize},
		Max: Point{X: i.bounds.Min.X + (x+1)*i.blockSize, Y: i.bounds.Min.Y + (y+1)*i.blockSize},
	}
	if rect.Max.X > i.bounds.Max.X {
		rect.Max.X = i.bounds.Max.X
	}
	if rect.Max.Y > i.bounds.Max.Y {
		rect.Max.Y = i.bounds.Max.Y
	}
	return rect
}

func (i *BlockIndex) Add(id int64, point Point) bool {
	block := i.blockAt(i.BlockIndex(point))
	if block == nil || id == 0 {
		return false
	}
	block.mu.Lock()
	_, existed := block.ids[id]
	if block.ids == nil {
		block.ids = make(map[int64]struct{})
	}
	block.ids[id] = struct{}{}
	if !existed {
		block.version++
	}
	block.mu.Unlock()
	return true
}

func (i *BlockIndex) Remove(id int64, point Point) bool {
	block := i.blockAt(i.BlockIndex(point))
	if block == nil || id == 0 {
		return false
	}
	block.mu.Lock()
	_, existed := block.ids[id]
	if existed {
		delete(block.ids, id)
		block.version++
	}
	block.mu.Unlock()
	return existed
}

func (i *BlockIndex) Move(id int64, from, to Point) bool {
	oldIndex, newIndex := i.BlockIndex(from), i.BlockIndex(to)
	oldBlock, newBlock := i.blockAt(oldIndex), i.blockAt(newIndex)
	if oldBlock == nil || newBlock == nil || id == 0 {
		return false
	}
	if oldBlock == newBlock {
		oldBlock.mu.Lock()
		_, exists := oldBlock.ids[id]
		if exists {
			oldBlock.version++
		}
		oldBlock.mu.Unlock()
		return exists
	}
	first, second := oldBlock, newBlock
	if newIndex < oldIndex {
		first, second = newBlock, oldBlock
	}
	first.mu.Lock()
	second.mu.Lock()
	_, exists := oldBlock.ids[id]
	if exists {
		delete(oldBlock.ids, id)
		if newBlock.ids == nil {
			newBlock.ids = make(map[int64]struct{})
		}
		newBlock.ids[id] = struct{}{}
		oldBlock.version++
		newBlock.version++
	}
	second.mu.Unlock()
	first.mu.Unlock()
	return exists
}

func (i *BlockIndex) QueryRect(rect Rect) []int64 {
	blocks, ok := i.BlockRects(rect)
	if !ok {
		return nil
	}
	return i.QueryBlocks(blocks)
}

func (i *BlockIndex) QueryBlock(point Point) []int64 {
	seen := make(map[int64]struct{})
	i.collect(i.BlockIndex(point), seen)
	return sortedIDs(seen)
}

func (i *BlockIndex) QueryBlocks(blocks map[int64]Rect) []int64 {
	seen := make(map[int64]struct{})
	for index := range blocks {
		i.collect(index, seen)
	}
	return sortedIDs(seen)
}

func (i *BlockIndex) RangeBlocks(blocks map[int64]Rect, fn func(int64) bool) bool {
	if i == nil || fn == nil {
		return true
	}
	indices := sortedBlockIndices(blocks)
	seen := make(map[int64]struct{})
	for _, index := range indices {
		block := i.blockAt(index)
		if block == nil {
			continue
		}
		block.mu.RLock()
		ids := make([]int64, 0, len(block.ids))
		for id := range block.ids {
			ids = append(ids, id)
		}
		block.mu.RUnlock()
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		for _, id := range ids {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			if !fn(id) {
				return false
			}
		}
	}
	return true
}

func (i *BlockIndex) BlockRects(rect Rect) (map[int64]Rect, bool) {
	rect, ok := i.ClampRect(rect)
	if !ok {
		return nil, false
	}
	startX := (rect.Min.X - i.bounds.Min.X) / i.blockSize
	startY := (rect.Min.Y - i.bounds.Min.Y) / i.blockSize
	endX := (rect.Max.X - 1 - i.bounds.Min.X) / i.blockSize
	endY := (rect.Max.Y - 1 - i.bounds.Min.Y) / i.blockSize
	out := make(map[int64]Rect, (endX-startX+1)*(endY-startY+1))
	for y := startY; y <= endY; y++ {
		for x := startX; x <= endX; x++ {
			index := y*i.cols + x
			out[index] = i.BlockRect(index)
		}
	}
	return out, len(out) > 0
}

func (i *BlockIndex) BlockVersions(blocks map[int64]Rect) map[int64]uint64 {
	out := make(map[int64]uint64, len(blocks))
	for index := range blocks {
		block := i.blockAt(index)
		if block == nil {
			continue
		}
		block.mu.RLock()
		out[index] = block.version
		block.mu.RUnlock()
	}
	return out
}

func (i *BlockIndex) ClampRect(rect Rect) (Rect, bool) {
	if i == nil {
		return Rect{}, false
	}
	rect = NormalizeRect(rect)
	if rect.Max.X <= i.bounds.Min.X || rect.Max.Y <= i.bounds.Min.Y || rect.Min.X >= i.bounds.Max.X || rect.Min.Y >= i.bounds.Max.Y {
		return Rect{}, false
	}
	if rect.Min.X < i.bounds.Min.X {
		rect.Min.X = i.bounds.Min.X
	}
	if rect.Min.Y < i.bounds.Min.Y {
		rect.Min.Y = i.bounds.Min.Y
	}
	if rect.Max.X > i.bounds.Max.X {
		rect.Max.X = i.bounds.Max.X
	}
	if rect.Max.Y > i.bounds.Max.Y {
		rect.Max.Y = i.bounds.Max.Y
	}
	return rect, !rect.Empty()
}

func (i *BlockIndex) Clear() {
	if i == nil {
		return
	}
	for _, block := range i.blocks {
		block.mu.Lock()
		if len(block.ids) > 0 {
			clear(block.ids)
			block.version++
		}
		block.mu.Unlock()
	}
}

func (i *BlockIndex) blockAt(index int64) *indexBlock {
	if i == nil || index < 0 || index >= int64(len(i.blocks)) {
		return nil
	}
	return i.blocks[index]
}

func (i *BlockIndex) collect(index int64, seen map[int64]struct{}) {
	block := i.blockAt(index)
	if block == nil {
		return
	}
	block.mu.RLock()
	for id := range block.ids {
		seen[id] = struct{}{}
	}
	block.mu.RUnlock()
}

func sortedIDs(seen map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

func sortedBlockIndices(blocks map[int64]Rect) []int64 {
	indices := make([]int64, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(a, b int) bool { return indices[a] < indices[b] })
	return indices
}

func divideCeil(value, by int64) int64 { return 1 + (value-1)/by }

func safeSpan(min, max int64) (int64, bool) {
	if max <= min {
		return 0, false
	}
	span := uint64(max) - uint64(min)
	const maxInt64 = uint64(^uint64(0) >> 1)
	if span > maxInt64 {
		return 0, false
	}
	return int64(span), true
}
