package spatial

// BlockLayout describes the bounds and row-major numbering of a BlockIndex,
// without allocating any per-block member or lock storage.
type BlockLayout struct {
	bounds                Rect
	blockSize, cols, rows int64
}

// NewBlockLayout validates the same bounds and logical block count as NewBlockIndex.
func NewBlockLayout(bounds Rect, blockSize int64) (BlockLayout, error) {
	bounds = NormalizeRect(bounds)
	if bounds.Empty() || blockSize <= 0 {
		return BlockLayout{}, ErrInvalidBounds
	}
	width, widthOK := SafeSpan(bounds.Min.X, bounds.Max.X)
	height, heightOK := SafeSpan(bounds.Min.Y, bounds.Max.Y)
	if !widthOK || !heightOK {
		return BlockLayout{}, ErrInvalidBounds
	}
	cols, rows := DivideCeil(width, blockSize), DivideCeil(height, blockSize)
	if cols <= 0 || rows <= 0 || cols > MaxBlockCount/rows {
		return BlockLayout{}, ErrTooManyBlocks
	}
	return BlockLayout{bounds: bounds, blockSize: blockSize, cols: cols, rows: rows}, nil
}

func (i *BlockLayout) Bounds() Rect { return i.bounds }

func (i *BlockLayout) BlockIndex(point Point) int64 {
	if i.blockSize <= 0 || !i.bounds.Contains(point) {
		return -1
	}
	x := (point.X - i.bounds.Min.X) / i.blockSize
	y := (point.Y - i.bounds.Min.Y) / i.blockSize
	return y*i.cols + x
}

func (i *BlockLayout) BlockRect(index int64) Rect {
	if index < 0 || index >= i.cols*i.rows {
		return Rect{}
	}
	x, y := index%i.cols, index/i.cols
	// The last block can be shorter; adding a whole block size can overflow.
	minX, minY := i.bounds.Min.X+x*i.blockSize, i.bounds.Min.Y+y*i.blockSize
	return Rect{
		Min: Point{X: minX, Y: minY},
		Max: Point{X: min(SaturatingAdd(minX, i.blockSize), i.bounds.Max.X), Y: min(SaturatingAdd(minY, i.blockSize), i.bounds.Max.Y)},
	}
}

func (i *BlockLayout) BlockRects(rect Rect) (map[int64]Rect, bool) {
	rect, ok := i.ClampRect(rect)
	if !ok {
		return nil, false
	}
	startX, startY, endX, endY := i.blockRange(rect)
	out := make(map[int64]Rect, (endX-startX+1)*(endY-startY+1))
	for y := startY; y <= endY; y++ {
		for x := startX; x <= endX; x++ {
			index := y*i.cols + x
			out[index] = i.BlockRect(index)
		}
	}
	return out, true
}

// blockRange takes a nonempty, clipped half-open rectangle.
func (i *BlockLayout) blockRange(rect Rect) (startX, startY, endX, endY int64) {
	return (rect.Min.X - i.bounds.Min.X) / i.blockSize,
		(rect.Min.Y - i.bounds.Min.Y) / i.blockSize,
		(rect.Max.X - 1 - i.bounds.Min.X) / i.blockSize,
		(rect.Max.Y - 1 - i.bounds.Min.Y) / i.blockSize
}

func (i *BlockLayout) ClampRect(rect Rect) (Rect, bool) {
	rect = NormalizeRect(rect)
	if rect.Max.X <= i.bounds.Min.X || rect.Max.Y <= i.bounds.Min.Y || rect.Min.X >= i.bounds.Max.X || rect.Min.Y >= i.bounds.Max.Y {
		return Rect{}, false
	}
	rect.Min.X, rect.Min.Y = max(rect.Min.X, i.bounds.Min.X), max(rect.Min.Y, i.bounds.Min.Y)
	rect.Max.X, rect.Max.Y = min(rect.Max.X, i.bounds.Max.X), min(rect.Max.Y, i.bounds.Max.Y)
	return rect, !rect.Empty()
}
