package cellworld

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 确定性夹具：Cell 不跑事件循环，测试在自己的 goroutine 上直接调用 handle / step，消息由测试决定何时投递、
// 丢弃、重复或扣住。这样“任一时刻至多一个 Real”可以在每次状态变化时精确检查。

const testBaseSID int32 = 900

// netMsg 是被截获的一条发布。
type netMsg struct {
	to   int32
	data []byte
	msg  *wireMsg
}

func (m netMsg) String() string {
	return fmt.Sprintf("%d->%d kind=%d", m.msg.From, m.to, m.msg.Kind)
}

// recorder 是只记录、不投递的 Transport。
type recorder struct {
	mu  sync.Mutex
	out []netMsg
}

func (r *recorder) Publish(subject string, data []byte) error {
	sid, err := strconv.ParseInt(subject[strings.LastIndexByte(subject, '.')+1:], 10, 32)
	if err != nil {
		return err
	}
	cp := append([]byte(nil), data...)
	m, err := decodeMsg(cp)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.out = append(r.out, netMsg{to: int32(sid), data: cp, msg: m})
	r.mu.Unlock()
	return nil
}

func (r *recorder) Subscribe(string, func([]byte)) (func() error, error) {
	return func() error { return nil }, nil
}

func (r *recorder) take() []netMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.out
	r.out = nil
	return out
}

// realChecker 在每次 RealChanged 时检查：同一实体同一时刻至多一个 Real，同一 (实体, epoch) 只被一个格取得，
// 每个实体被取得的 epoch 严格递增。
type realChecker struct {
	t        testing.TB
	mu       sync.Mutex
	holders  map[int64]map[CellID]uint64
	acquired map[[2]uint64]CellID
	lastEp   map[int64]uint64
	count    map[CellID]int
}

func newRealChecker(t testing.TB) *realChecker {
	return &realChecker{t: t, holders: map[int64]map[CellID]uint64{}, acquired: map[[2]uint64]CellID{}, lastEp: map[int64]uint64{}, count: map[CellID]int{}}
}

func (rc *realChecker) hook(cell CellID, id int64, epoch uint64, acquired bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	h := rc.holders[id]
	if h == nil {
		h = map[CellID]uint64{}
		rc.holders[id] = h
	}
	if !acquired {
		delete(h, cell)
		return
	}
	rc.count[cell]++
	if len(h) > 0 {
		rc.t.Errorf("entity %d: cell %v acquired Real at epoch %d while %v still Real", id, cell, epoch, h)
	}
	key := [2]uint64{uint64(id), epoch}
	if prev, dup := rc.acquired[key]; dup {
		rc.t.Errorf("entity %d epoch %d acquired twice: %v then %v", id, epoch, prev, cell)
	}
	rc.acquired[key] = cell
	if epoch <= rc.lastEp[id] {
		rc.t.Errorf("entity %d: epoch went %d -> %d", id, rc.lastEp[id], epoch)
	}
	rc.lastEp[id] = epoch
	h[cell] = epoch
}

func (rc *realChecker) acquisitions(cell CellID) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.count[cell]
}

type harness struct {
	t      *testing.T
	grid   *Grid
	rec    *recorder
	cells  map[int32]*Cell
	now    time.Time
	period time.Duration
	base   []*wireMsg // 投递给 testBaseSID 的消息
	real   *realChecker
	hits   map[HitID]int
	held   []netMsg
}

// newHarness 给网格的每个格建一个 Cell；mod 可改每个格的配置。
func newHarness(t *testing.T, g *Grid, mod func(*CellConfig)) *harness {
	h := &harness{t: t, grid: g, rec: &recorder{}, cells: map[int32]*Cell{}, now: time.Unix(1_700_000_000, 0), period: 50 * time.Millisecond,
		real: newRealChecker(t), hits: map[HitID]int{}}
	for _, id := range g.Cells() {
		cfg := CellConfig{World: "t", Grid: g, Cell: id, Transport: h.rec, ViewRadius: 80, Period: h.period}
		cfg.Hooks.RealChanged = h.real.hook
		cfg.Hooks.DamageApplied = func(_ CellID, _ int64, hit HitID, _ int32) { h.hits[hit]++ }
		if mod != nil {
			mod(&cfg)
		}
		c, err := NewCell(cfg)
		if err != nil {
			t.Fatal(err)
		}
		h.cells[c.SID()] = c
	}
	return h
}

func (h *harness) cell(id CellID) *Cell { return h.cells[h.grid.SID(id)] }

// deliver 投递一批消息：发给 Cell 的直接 handle，发给 Base 的记下来。
func (h *harness) deliver(ms []netMsg) {
	for _, m := range ms {
		if c, ok := h.cells[m.to]; ok {
			fresh, err := decodeMsg(m.data) // 每次投递重新解码，重复投递互不共享对象
			if err != nil {
				h.t.Fatal(err)
			}
			c.handle(fresh, h.now)
			continue
		}
		if m.to == testBaseSID {
			h.base = append(h.base, m.msg)
		}
	}
}

// verdict 决定一条消息的命运。
type verdict int

const (
	pass verdict = iota
	drop
	dup
	hold // 扣住，之后由测试 release
)

// pump 反复取出并投递，直到没有新消息；filter 为 nil 时全部放行。
func (h *harness) pump(filter func(netMsg) verdict) {
	for range 100 {
		ms := h.rec.take()
		if len(ms) == 0 {
			return
		}
		var out []netMsg
		for _, m := range ms {
			v := pass
			if filter != nil {
				v = filter(m)
			}
			switch v {
			case pass:
				out = append(out, m)
			case dup:
				out = append(out, m, m)
			case hold:
				h.held = append(h.held, m)
			}
		}
		h.deliver(out)
	}
	h.t.Fatal("pump did not quiesce")
}

func (h *harness) release() {
	held := h.held
	h.held = nil
	h.deliver(held)
}

// step 让时间前进一拍，所有格各推进一拍，然后按 filter 投递。
func (h *harness) step(filter func(netMsg) verdict) {
	h.now = h.now.Add(h.period)
	for _, id := range h.grid.Cells() {
		h.cell(id).step(h.now)
	}
	h.pump(filter)
}

func (h *harness) roles(id int64) map[CellID]string {
	out := map[CellID]string{}
	for _, cid := range h.grid.Cells() {
		if e, ok := h.cell(cid).ents[id]; ok {
			out[cid] = e.role.String()
		}
	}
	return out
}

func (h *harness) realAt(id int64) (CellID, *entity, bool) {
	var found []CellID
	var got *entity
	for _, cid := range h.grid.Cells() {
		if e, ok := h.cell(cid).ents[id]; ok && e.role == roleReal {
			found = append(found, cid)
			got = e
		}
	}
	if len(found) > 1 {
		h.t.Fatalf("entity %d is Real in %v", id, found)
	}
	if len(found) == 0 {
		return CellID{}, nil, false
	}
	return found[0], got, true
}

func (h *harness) spawn(id CellID, st EntityState) {
	if err := h.cell(id).spawn(st, h.now); err != nil {
		h.t.Fatal(err)
	}
	h.pump(nil)
}

// enterPlayer 模拟 Base 发 Enter。
func (h *harness) enterPlayer(id int64, at spatial.Point, hp int32) {
	cellID, ok := h.grid.CellAt(at)
	if !ok {
		h.t.Fatalf("%v outside world", at)
	}
	h.cell(cellID).handle(&wireMsg{Kind: kindEnter, From: testBaseSID, Enter: &enterReq{
		State: EntityState{ID: id, X: at.X, Y: at.Y, HP: hp}, EpochHint: 1,
	}}, h.now)
	h.pump(nil)
}

func (h *harness) baseMsgs(kind msgKind) []*wireMsg {
	var out []*wireMsg
	for _, m := range h.base {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func kindIs(kinds ...msgKind) func(netMsg) bool {
	return func(m netMsg) bool {
		for _, k := range kinds {
			if m.msg.Kind == k {
				return true
			}
		}
		return false
	}
}

func testGrid1x2(t *testing.T) *Grid {
	t.Helper()
	g, err := NewGrid(GridConfig{CellSize: 1000, Cols: 2, Rows: 1, GhostBand: 100, Hysteresis: 20,
		Owners: map[CellID]int32{{0, 0}: 101, {1, 0}: 102}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
