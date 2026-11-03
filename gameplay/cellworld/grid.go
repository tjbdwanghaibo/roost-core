package cellworld

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ErrGridConfig 表示网格配置不合法（尺寸、边界带、owner 映射）。
var ErrGridConfig = errors.New("cellworld: invalid grid config")

// CellID 是网格里的一个格（一个 Cell 负责的区域），列号 Col 沿 X、行号 Row 沿 Y，从 0 开始。
type CellID struct {
	Col int32 `msgpack:"c"`
	Row int32 `msgpack:"r"`
}

func (id CellID) String() string { return fmt.Sprintf("(%d,%d)", id.Col, id.Row) }

// GridConfig 是静态分区：所有进程读同一份，格 → Cell 进程 sid 的映射也在这里（v1 不做动态分区）。
// 坐标是整数世界单位（测试与测量里 1 单位 = 1 厘米）。
type GridConfig struct {
	// Origin 是网格左下角（最小 X、Y）。
	Origin spatial.Point
	// CellSize 是方格边长，必须为正。
	CellSize int64
	// Cols、Rows 是列数与行数，都必须为正。
	Cols, Rows int32
	// GhostBand 是边界带宽度 G：Real 落在某邻格外扩 G 的矩形内就向它发 Ghost。
	// 必须为正且小于 CellSize（只镜像到相邻格）；Cell 还要求它不小于视野半径。
	GhostBand int64
	// Hysteresis 是交接迟滞 H：Real 越出本格超过 H 才交接。必须 0 ≤ H < GhostBand，
	// 这样越界未交接的 Real 一定在目标格的 Ghost 范围里。
	Hysteresis int64
	// Owners 给每个格指定负责它的 Cell 进程 sid（非零），必须覆盖全部格；一个 sid 只能负责一个格
	// （v1 每个 Cell 进程一个格，收件主题按 sid 区分）。
	Owners map[CellID]int32
}

// Grid 是校验过的静态分区，构造后只读，可在 goroutine 间共享。
type Grid struct {
	origin     spatial.Point
	size       int64
	cols, rows int32
	band       int64
	hysteresis int64
	owners     map[CellID]int32
	cellOf     map[int32]CellID
	bounds     spatial.Rect
}

// NewGrid 校验配置并构造 Grid。
func NewGrid(cfg GridConfig) (*Grid, error) {
	if cfg.CellSize <= 0 || cfg.Cols <= 0 || cfg.Rows <= 0 {
		return nil, fmt.Errorf("%w: cell size %d, cols %d, rows %d must be positive", ErrGridConfig, cfg.CellSize, cfg.Cols, cfg.Rows)
	}
	if cfg.GhostBand <= 0 || cfg.GhostBand >= cfg.CellSize {
		return nil, fmt.Errorf("%w: ghost band %d must be in (0, cell size %d)", ErrGridConfig, cfg.GhostBand, cfg.CellSize)
	}
	if cfg.Hysteresis < 0 || cfg.Hysteresis >= cfg.GhostBand {
		return nil, fmt.Errorf("%w: hysteresis %d must be in [0, ghost band %d)", ErrGridConfig, cfg.Hysteresis, cfg.GhostBand)
	}
	// 整个世界的跨度不能溢出 int64；保守地要求每条边不超过 MaxInt64/4。
	if int64(cfg.Cols) > math.MaxInt64/4/cfg.CellSize || int64(cfg.Rows) > math.MaxInt64/4/cfg.CellSize {
		return nil, fmt.Errorf("%w: world too large", ErrGridConfig)
	}
	g := &Grid{
		origin: cfg.Origin, size: cfg.CellSize, cols: cfg.Cols, rows: cfg.Rows,
		band: cfg.GhostBand, hysteresis: cfg.Hysteresis,
		owners: make(map[CellID]int32, len(cfg.Owners)), cellOf: make(map[int32]CellID, len(cfg.Owners)),
	}
	g.bounds = spatial.Rect{Min: cfg.Origin, Max: spatial.Point{
		X: spatial.SaturatingAdd(cfg.Origin.X, int64(cfg.Cols)*cfg.CellSize),
		Y: spatial.SaturatingAdd(cfg.Origin.Y, int64(cfg.Rows)*cfg.CellSize),
	}}
	for col := int32(0); col < cfg.Cols; col++ {
		for row := int32(0); row < cfg.Rows; row++ {
			id := CellID{Col: col, Row: row}
			sid := cfg.Owners[id]
			if sid == 0 {
				return nil, fmt.Errorf("%w: cell %v has no owner sid", ErrGridConfig, id)
			}
			if other, dup := g.cellOf[sid]; dup {
				return nil, fmt.Errorf("%w: sid %d owns both %v and %v", ErrGridConfig, sid, other, id)
			}
			g.owners[id] = sid
			g.cellOf[sid] = id
		}
	}
	if len(cfg.Owners) != len(g.owners) {
		return nil, fmt.Errorf("%w: owners name cells outside the %dx%d grid", ErrGridConfig, cfg.Cols, cfg.Rows)
	}
	return g, nil
}

// Bounds 是整个世界的矩形（半开）。
func (g *Grid) Bounds() spatial.Rect { return g.bounds }

// GhostBand 返回 G。
func (g *Grid) GhostBand() int64 { return g.band }

// Hysteresis 返回 H。
func (g *Grid) Hysteresis() int64 { return g.hysteresis }

// Contains 报告 id 是不是本网格里的格。
func (g *Grid) Contains(id CellID) bool {
	return id.Col >= 0 && id.Col < g.cols && id.Row >= 0 && id.Row < g.rows
}

// CellAt 返回点所在的格；世界之外返回 false。格是半开区间，边界线上的点属于右 / 上侧的格。
func (g *Grid) CellAt(p spatial.Point) (CellID, bool) {
	if !g.bounds.Contains(p) {
		return CellID{}, false
	}
	return CellID{Col: int32((p.X - g.origin.X) / g.size), Row: int32((p.Y - g.origin.Y) / g.size)}, true
}

// Rect 返回格的矩形（半开）；格不在网格里返回空矩形。
func (g *Grid) Rect(id CellID) spatial.Rect {
	if !g.Contains(id) {
		return spatial.Rect{}
	}
	minX := g.origin.X + int64(id.Col)*g.size
	minY := g.origin.Y + int64(id.Row)*g.size
	return spatial.Rect{Min: spatial.Point{X: minX, Y: minY}, Max: spatial.Point{X: minX + g.size, Y: minY + g.size}}
}

// SID 返回负责该格的 Cell 进程 sid；格不在网格里返回 0。
func (g *Grid) SID(id CellID) int32 { return g.owners[id] }

// CellOf 返回 sid 负责的格。
func (g *Grid) CellOf(sid int32) (CellID, bool) {
	id, ok := g.cellOf[sid]
	return id, ok
}

// Cells 按 (Row, Col) 顺序返回全部格。
func (g *Grid) Cells() []CellID {
	out := make([]CellID, 0, len(g.owners))
	for id := range g.owners {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// expand 把矩形四边各外扩 by。
func expand(r spatial.Rect, by int64) spatial.Rect {
	return spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(r.Min.X, by), Y: spatial.SaturatingSub(r.Min.Y, by)},
		Max: spatial.Point{X: spatial.SaturatingAdd(r.Max.X, by), Y: spatial.SaturatingAdd(r.Max.Y, by)},
	}
}

// GhostTargets 把“owner 格的 Real 在 p 处时要向哪些格发 Ghost”追加到 dst：owner 之外、外扩 G 后包含 p 的格。
// 只检查 owner 周围 8 格（G < CellSize 保证更远的格不可能命中），按 (Row, Col) 顺序，四格交点处最多 3 个。
// Real 已越出本格（迟滞内）时，它所在的那个邻格同样命中。
func (g *Grid) GhostTargets(owner CellID, p spatial.Point, dst []CellID) []CellID {
	for dr := int32(-1); dr <= 1; dr++ {
		for dc := int32(-1); dc <= 1; dc++ {
			if dr == 0 && dc == 0 {
				continue
			}
			id := CellID{Col: owner.Col + dc, Row: owner.Row + dr}
			if g.Contains(id) && expand(g.Rect(id), g.band).Contains(p) {
				dst = append(dst, id)
			}
		}
	}
	return dst
}

// InBand 报告 p 是否在 id 格外扩 G 的矩形内：Real 交接走后旧格据此决定降为 Ghost 还是删除。
func (g *Grid) InBand(id CellID, p spatial.Point) bool {
	return g.Contains(id) && expand(g.Rect(id), g.band).Contains(p)
}

// ShouldHandoff 报告 owner 格的 Real 走到 p 时是否该交接，以及交给哪个格：越出本格外扩 H 的矩形、
// 且 p 仍在世界内。世界之外的点不交接（Cell 会把位置夹回世界内）。
func (g *Grid) ShouldHandoff(owner CellID, p spatial.Point) (CellID, bool) {
	if expand(g.Rect(owner), g.hysteresis).Contains(p) {
		return CellID{}, false
	}
	target, ok := g.CellAt(p)
	if !ok || target == owner {
		return CellID{}, false
	}
	return target, true
}
