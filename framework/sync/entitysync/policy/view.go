package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ViewID 是视野的逻辑编号，由业务分配与解释（联盟号、副本阵营、眼位、单位……），框架不解析，也不要求它是实体 ID：
// 视野不绑定实体，空间视野可以跟随一个实体，跟随关系可改、可为空。
//
// 0 保留给玩家主视野（MainView）：业务视野不能用 0；主视野没有全局编号，只在按玩家的入口里出现。
type ViewID int64

// MainView 在按玩家的入口与说明里指玩家自己的主视野（Enter / Watch 建立的镜头）。
const MainView ViewID = 0

// 视野错误。
var (
	// ErrViewUnknown：视野没有定义（或已移除）。
	ErrViewUnknown = errors.New("policy: unknown view")
	// ErrViewExists：同一个 ViewID 已经定义过；先 RemoveView 再定义。
	ErrViewExists = errors.New("policy: view already defined")
	// ErrViewKind：对关系视野做空间操作，或反过来。
	ErrViewKind = errors.New("policy: operation does not fit the view's kind")
	// ErrViewLimit：挂接超过 MaxViewsPerPlayer 或 MaxPlayersPerView。
	ErrViewLimit = errors.New("policy: view attachment limit reached")
)

// viewLimitMetric 计数被上限拒绝的挂接，reason 为 views_per_player 或 players_per_view。
const viewLimitMetric = "entitysync_interest_view_limit_rejected_total"

// view 是登记表里的一个视野。登记表是视野、挂接与成员的唯一事实，由 viewMu 保护；应用侧（mu）只保存 AOI 状态与
// 已生效的空间视野挂接（spatialPlayers）。
type view struct {
	id     ViewID
	handle int64 // 内部编号：空间视野是 AOI 观察者编号，也是 held 里的来源键
	owner  int64 // 主视野所属玩家；业务视野为 0
	// 空间视野：最近一次显式给的视点、归一后的视野与跟随实体（0 为不跟随）。跟随实体在场时视点取实体位置。
	spatial bool
	at      spatial.Point
	sight   ObserverView
	follow  int64
	// 关系视野：给出的层级与成员（实体 ID）。
	layer   entity.SyncProfile
	members map[int64]struct{}
	players map[int64]struct{} // 挂接的玩家
}

type viewOpKind uint8

const (
	// opRelation：players × subjects 的关系贡献进入（enter）或离开。由登记表在变更的那一刻按当时的事实展开，
	// 应用时不再读登记表，所以先后两次变更之间的顺序就是调用顺序。
	opRelation viewOpKind = iota
	// opSpatialPlace：定义、移动、换视野或换跟随之后空间视野的完整参数。
	opSpatialPlace
	opSpatialAttach
	opSpatialDetach
	opSpatialRemove
)

type viewOp struct {
	kind     viewOpKind
	handle   int64
	enter    bool
	layer    entity.SyncProfile
	players  []int64
	subjects []int64
	player   int64
	at       spatial.Point
	sight    ObserverView
	follow   int64
	// positioned：at 是这次显式给的视点（定义、MoveSpatialView）；为假时（换视野、换跟随）视点留在 AOI 里的当前位置，
	// 例如不再跟随时停在跟随到的地方，而不是回到很久以前的显式位置。
	positioned bool
}

// viewRegistry 是视野登记表：业务的视野操作只取 mu（viewMu），改事实、记一条 viewOp，不等待空间计算与 Manager；
// Interest 在下一次 Apply / 政策阶段（持 Interest.mu）按顺序应用 ops。
type viewRegistry struct {
	mu         sync.Mutex
	closed     bool
	views      map[ViewID]*view
	mains      map[int64]*view
	attached   map[int64]map[*view]struct{} // 玩家 → 挂接的视野（含主视野）
	memberOf   map[int64]map[*view]struct{} // 实体 → 含它的关系视野
	nextHandle int64
	ops        []viewOp
	// active：记过业务视野变更之后为真，此后 Manager 的政策阶段结算并重试本 Interest（与排队事实同一条规则）。
	active     bool
	maxViews   int
	maxPlayers int
	wake       func()
}

func (r *viewRegistry) init(maxViews, maxPlayers int) {
	r.views = make(map[ViewID]*view)
	r.mains = make(map[int64]*view)
	r.attached = make(map[int64]map[*view]struct{})
	r.memberOf = make(map[int64]map[*view]struct{})
	r.maxViews, r.maxPlayers = maxViews, maxPlayers
}

func (r *viewRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.ops = nil
}

// record 追加一条变更；唤醒 Manager 的政策阶段，排队模式下视野变化与事实同一批说给 Manager。调用方持有 r.mu。
func (r *viewRegistry) record(op viewOp) {
	r.active = true
	r.ops = append(r.ops, op)
	if len(r.ops) == 1 && r.wake != nil {
		r.wake()
	}
}

// state 报告是否有未应用的变更，以及是否记过业务视野变更。
func (r *viewRegistry) state() (pending, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ops) > 0, r.active
}

func (r *viewRegistry) take() []viewOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	ops := r.ops
	r.ops = nil
	return ops
}

func (r *viewRegistry) newHandle() int64 {
	r.nextHandle++
	return r.nextHandle
}

func (r *viewRegistry) mainOf(playerID int64) *view {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mains[playerID]
}

// mainView 返回玩家的主视野，没有就建一个（计入 MaxViewsPerPlayer）。主视野的放置由调用方（持 Interest.mu）直接做，不记 op。
func (r *viewRegistry) mainView(playerID int64) (*view, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("policy: interest is closed")
	}
	if main := r.mains[playerID]; main != nil {
		return main, nil
	}
	if len(r.attached[playerID]) >= r.maxViews {
		metrics.IncCounter(viewLimitMetric, metrics.Labels{"reason": "views_per_player"}, 1)
		return nil, fmt.Errorf("%w: player %d already has %d views", ErrViewLimit, playerID, r.maxViews)
	}
	main := &view{handle: r.newHandle(), owner: playerID, spatial: true, players: map[int64]struct{}{playerID: {}}}
	r.mains[playerID] = main
	r.attachLocked(playerID, main)
	return main, nil
}

func (r *viewRegistry) attachLocked(playerID int64, v *view) {
	views := r.attached[playerID]
	if views == nil {
		views = make(map[*view]struct{})
		r.attached[playerID] = views
	}
	views[v] = struct{}{}
	v.players[playerID] = struct{}{}
}

func (r *viewRegistry) detachLocked(playerID int64, v *view) {
	delete(v.players, playerID)
	if views := r.attached[playerID]; views != nil {
		delete(views, v)
		if len(views) == 0 {
			delete(r.attached, playerID)
		}
	}
	if v.spatial {
		r.record(viewOp{kind: opSpatialDetach, handle: v.handle, player: playerID})
	} else if len(v.members) > 0 {
		r.record(viewOp{kind: opRelation, handle: v.handle, players: []int64{playerID}, subjects: sortedIDs(v.members)})
	}
}

// leave 解除玩家的全部挂接、删掉主视野（返回它，由调用方撤出 AOI），并把实体从所有关系视野里去掉。
func (r *viewRegistry) leave(playerID, entityID int64) *view {
	r.mu.Lock()
	defer r.mu.Unlock()
	main := r.mains[playerID]
	delete(r.mains, playerID)
	views := make([]*view, 0, len(r.attached[playerID]))
	for v := range r.attached[playerID] {
		views = append(views, v)
	}
	slices.SortFunc(views, func(a, b *view) int { return cmp.Compare(a.handle, b.handle) })
	for _, v := range views {
		if v == main {
			delete(v.players, playerID)
			delete(r.attached[playerID], v)
			continue
		}
		r.detachLocked(playerID, v)
	}
	delete(r.attached, playerID)
	if entityID != 0 {
		r.forgetLocked(entityID)
	}
	return main
}

func (r *viewRegistry) forget(entityID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetLocked(entityID)
}

// forgetLocked 把实体从所有关系视野的成员里去掉（Hide / Leave）：关系视野本身保留。
func (r *viewRegistry) forgetLocked(entityID int64) {
	views := r.memberOf[entityID]
	delete(r.memberOf, entityID)
	ordered := make([]*view, 0, len(views))
	for v := range views {
		ordered = append(ordered, v)
	}
	slices.SortFunc(ordered, func(a, b *view) int { return cmp.Compare(a.handle, b.handle) })
	for _, v := range ordered {
		delete(v.members, entityID)
		if len(v.players) > 0 {
			r.record(viewOp{kind: opRelation, handle: v.handle, players: sortedIDs(v.players), subjects: []int64{entityID}})
		}
	}
}

// lookup 取业务视野，并检查种类。调用方持有 r.mu。
func (r *viewRegistry) lookup(id ViewID, spatialView bool) (*view, error) {
	if r.closed {
		return nil, errors.New("policy: interest is closed")
	}
	v := r.views[id]
	if v == nil {
		return nil, fmt.Errorf("%w: %d", ErrViewUnknown, id)
	}
	if v.spatial != spatialView {
		return nil, fmt.Errorf("%w: view %d", ErrViewKind, id)
	}
	return v, nil
}

func (r *viewRegistry) define(v *view) error {
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	if v.id == MainView {
		return fmt.Errorf("%w: view id 0 is reserved for the main view", ErrInterestConfig)
	}
	if r.views[v.id] != nil {
		return fmt.Errorf("%w: %d", ErrViewExists, v.id)
	}
	v.handle = r.newHandle()
	v.players = make(map[int64]struct{})
	r.views[v.id] = v
	return nil
}

func (r *viewRegistry) recordPlace(v *view, positioned bool) {
	r.record(viewOp{kind: opSpatialPlace, handle: v.handle, at: v.at, sight: v.sight, follow: v.follow, positioned: positioned})
}

// ---- 业务视野 API（只取 viewMu，不进 Nest、不排队、不等待空间计算） ----
//
// 视野的增删改、成员变更与挂接都是直接调用：参数在调用时校验并写进登记表，效果由下一次 Apply（或排队模式下 Manager 的
// 政策阶段）按调用顺序应用。它们不走提交门：事务里决定的视野变化（入盟、放眼）由业务在提交之后调用。

// DefineSpatialView 定义一个空间视野：视点 at、视野 sight（LOD 层级与视野大小，归一规则同 SetObserverView，零值是缺省视野）、
// 可选跟随实体 follow（0 为不跟随）。跟随的实体在场时视点取它的位置并随它移动，它自己不进这个视野的可见集。
// 定义之后用 AttachView 挂给玩家；MaxVisible 与块预算按视野计。
func (in *Interest) DefineSpatialView(id ViewID, at spatial.Point, sight ObserverView, follow int64) error {
	if !in.aoi.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	normalized, err := in.aoi.config.normalizeView(sight)
	if err != nil {
		return err
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &view{id: id, spatial: true, at: at, sight: normalized, follow: follow}
	if err := r.define(v); err != nil {
		return err
	}
	r.recordPlace(v, true)
	return nil
}

// MoveSpatialView 移动空间视野的视点。跟随实体的视野在实体在场时以实体位置为准，实体下一次移动会把它带回去。
func (in *Interest) MoveSpatialView(id ViewID, to spatial.Point) error {
	if !in.aoi.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, true)
	if err != nil {
		return err
	}
	v.at = to
	r.recordPlace(v, true)
	return nil
}

// SetSpatialView 换空间视野的 LOD 层级与视野大小（服务端设置，不限速），返回归一后的视野；层级未配置返回 ErrObserverView。
func (in *Interest) SetSpatialView(id ViewID, sight ObserverView) (ObserverView, error) {
	normalized, err := in.aoi.config.normalizeView(sight)
	if err != nil {
		return ObserverView{}, err
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, true)
	if err != nil {
		return ObserverView{}, err
	}
	if v.sight != normalized {
		v.sight = normalized
		r.recordPlace(v, false)
	}
	return normalized, nil
}

// SetViewFollow 换空间视野跟随的实体（0 为不再跟随，视点留在当前位置）。新跟随的实体在场时视点跳到它的位置。
func (in *Interest) SetViewFollow(id ViewID, follow int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, true)
	if err != nil {
		return err
	}
	if v.follow != follow {
		v.follow = follow
		r.recordPlace(v, false)
	}
	return nil
}

// DefineRelationView 定义一个关系视野：成员（实体 ID）由 SetViewMembers / AddViewMembers / RemoveViewMembers 维护，
// 挂接它的玩家看见全部成员，不论距离；layer 是它给的同步层级（零值是缺省视图）。配置了 ViewSets 时层级必须已声明。
func (in *Interest) DefineRelationView(id ViewID, layer entity.SyncProfile) error {
	layer = layer.Normalize()
	if err := in.validateProfile(layer); err != nil {
		return fmt.Errorf("policy: relation view %d layer: %w", id, err)
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.define(&view{id: id, layer: layer, members: make(map[int64]struct{})})
}

// SetViewMembers 替换关系视野的成员；差量在这里算，挂接的玩家只收到变化的部分。零 ID 忽略。
func (in *Interest) SetViewMembers(id ViewID, subjects []int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, false)
	if err != nil {
		return err
	}
	next := make(map[int64]struct{}, len(subjects))
	for _, subject := range subjects {
		if subject != 0 {
			next[subject] = struct{}{}
		}
	}
	var removed, added []int64
	for subject := range v.members {
		if _, kept := next[subject]; !kept {
			removed = append(removed, subject)
		}
	}
	for subject := range next {
		if _, had := v.members[subject]; !had {
			added = append(added, subject)
		}
	}
	r.changeMembersLocked(v, added, removed)
	return nil
}

// AddViewMembers 给关系视野加成员；已在的忽略。
func (in *Interest) AddViewMembers(id ViewID, subjects ...int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, false)
	if err != nil {
		return err
	}
	var added []int64
	seen := make(map[int64]struct{}, len(subjects))
	for _, subject := range subjects {
		_, had := v.members[subject]
		_, repeated := seen[subject]
		if subject != 0 && !had && !repeated {
			seen[subject] = struct{}{}
			added = append(added, subject)
		}
	}
	r.changeMembersLocked(v, added, nil)
	return nil
}

// RemoveViewMembers 从关系视野去掉成员；不在的忽略。
func (in *Interest) RemoveViewMembers(id ViewID, subjects ...int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.lookup(id, false)
	if err != nil {
		return err
	}
	var removed []int64
	seen := make(map[int64]struct{}, len(subjects))
	for _, subject := range subjects {
		_, had := v.members[subject]
		_, repeated := seen[subject]
		if had && !repeated {
			seen[subject] = struct{}{}
			removed = append(removed, subject)
		}
	}
	r.changeMembersLocked(v, nil, removed)
	return nil
}

// changeMembersLocked 更新成员与反查表，并按当时挂接的玩家记两条关系变更（先离开后进入）。
func (r *viewRegistry) changeMembersLocked(v *view, added, removed []int64) {
	for _, subject := range removed {
		delete(v.members, subject)
		if views := r.memberOf[subject]; views != nil {
			delete(views, v)
			if len(views) == 0 {
				delete(r.memberOf, subject)
			}
		}
	}
	for _, subject := range added {
		v.members[subject] = struct{}{}
		views := r.memberOf[subject]
		if views == nil {
			views = make(map[*view]struct{})
			r.memberOf[subject] = views
		}
		views[v] = struct{}{}
	}
	if len(v.players) == 0 {
		return
	}
	players := sortedIDs(v.players)
	if len(removed) > 0 {
		slices.Sort(removed)
		r.record(viewOp{kind: opRelation, handle: v.handle, players: players, subjects: removed})
	}
	if len(added) > 0 {
		slices.Sort(added)
		r.record(viewOp{kind: opRelation, handle: v.handle, enter: true, layer: v.layer, players: players, subjects: added})
	}
}

// AttachView 把视野挂给玩家：玩家此后看见视野看见的一切。已挂接的再挂一次什么也不做；
// 超过 MaxViewsPerPlayer（含主视野）或 MaxPlayersPerView 返回 ErrViewLimit 并计数。主视野（MainView）不能挂接。
func (in *Interest) AttachView(playerID int64, id ViewID) error {
	if playerID == 0 {
		return ErrInterestUnknown
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	v := r.views[id]
	if v == nil {
		return fmt.Errorf("%w: %d", ErrViewUnknown, id)
	}
	if _, attached := v.players[playerID]; attached {
		return nil
	}
	if len(r.attached[playerID]) >= r.maxViews {
		metrics.IncCounter(viewLimitMetric, metrics.Labels{"reason": "views_per_player"}, 1)
		return fmt.Errorf("%w: player %d already has %d views", ErrViewLimit, playerID, r.maxViews)
	}
	if len(v.players) >= r.maxPlayers {
		metrics.IncCounter(viewLimitMetric, metrics.Labels{"reason": "players_per_view"}, 1)
		return fmt.Errorf("%w: view %d already has %d players", ErrViewLimit, id, r.maxPlayers)
	}
	r.attachLocked(playerID, v)
	if v.spatial {
		r.record(viewOp{kind: opSpatialAttach, handle: v.handle, player: playerID})
	} else if len(v.members) > 0 {
		r.record(viewOp{kind: opRelation, handle: v.handle, enter: true, layer: v.layer, players: []int64{playerID}, subjects: sortedIDs(v.members)})
	}
	return nil
}

// DetachView 解除挂接；没挂接的什么也不做。别的视野仍持有的目标不退订。
func (in *Interest) DetachView(playerID int64, id ViewID) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	v := r.views[id]
	if v == nil {
		return fmt.Errorf("%w: %d", ErrViewUnknown, id)
	}
	if _, attached := v.players[playerID]; attached {
		r.detachLocked(playerID, v)
	}
	return nil
}

// RemoveView 移除视野，并自动从所有挂接它的玩家解除；之后同一个 ViewID 可以重新定义。
func (in *Interest) RemoveView(id ViewID) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	v := r.views[id]
	if v == nil {
		return fmt.Errorf("%w: %d", ErrViewUnknown, id)
	}
	delete(r.views, id)
	players := sortedIDs(v.players)
	for _, playerID := range players {
		delete(v.players, playerID)
		if views := r.attached[playerID]; views != nil {
			delete(views, v)
			if len(views) == 0 {
				delete(r.attached, playerID)
			}
		}
	}
	if v.spatial {
		r.record(viewOp{kind: opSpatialRemove, handle: v.handle})
		return nil
	}
	if len(players) > 0 && len(v.members) > 0 {
		r.record(viewOp{kind: opRelation, handle: v.handle, players: players, subjects: sortedIDs(v.members)})
	}
	for subject := range v.members {
		if views := r.memberOf[subject]; views != nil {
			delete(views, v)
			if len(views) == 0 {
				delete(r.memberOf, subject)
			}
		}
	}
	return nil
}

// ViewPlayers 返回挂接视野的玩家（升序）；视野不存在时 ok 为 false。
func (in *Interest) ViewPlayers(id ViewID) (players []int64, ok bool) {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.views[id]
	if v == nil {
		return nil, false
	}
	return sortedIDs(v.players), true
}

// ViewMembers 返回关系视野的成员（升序）；视野不存在或不是关系视野时 ok 为 false。
func (in *Interest) ViewMembers(id ViewID) (members []int64, ok bool) {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.views[id]
	if v == nil || v.spatial {
		return nil, false
	}
	return sortedIDs(v.members), true
}

// ViewVisible 返回空间视野当前按距离看见的目标（升序）；视野不存在或不是空间视野时返回 nil。
func (in *Interest) ViewVisible(id ViewID) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.syncViewsLocked()
	r := &in.registry
	r.mu.Lock()
	v := r.views[id]
	r.mu.Unlock()
	if v == nil || !v.spatial {
		return nil
	}
	return in.aoi.Visible(v.handle)
}

func sortedIDs(set map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ---- 应用侧（持 Interest.mu） ----

// syncViewsLocked 按调用顺序应用登记表里的变更。每个持 mu 的入口先调用它，保证直接操作（Enter、Move……）看到的
// 视野与调用顺序一致；它不在 AOI 的批次写阶段内调用。
func (in *Interest) syncViewsLocked() {
	ops := in.registry.take()
	for _, op := range ops {
		switch op.kind {
		case opRelation:
			source := contribution{view: op.handle}
			for _, playerID := range op.players {
				for _, subject := range op.subjects {
					in.contribute(pair{observer: playerID, subject: subject}, source, op.layer, op.enter)
				}
			}
		case opSpatialPlace:
			if in.spatialPlayers[op.handle] == nil {
				in.spatialPlayers[op.handle] = make(map[int64]struct{})
			}
			at := op.at
			if observer := in.aoi.observers[op.handle]; !op.positioned && observer != nil {
				at = observer.at
			}
			in.placeViewLocked(op.handle, op.follow, at, op.sight)
		case opSpatialAttach:
			in.drainAOILocked()
			players := in.spatialPlayers[op.handle]
			if players == nil {
				continue
			}
			players[op.player] = struct{}{}
			in.seedSpatialLocked(op.player, op.handle, true)
		case opSpatialDetach:
			in.drainAOILocked()
			if players := in.spatialPlayers[op.handle]; players != nil {
				delete(players, op.player)
				in.seedSpatialLocked(op.player, op.handle, false)
			}
		case opSpatialRemove:
			in.removeSpatialLocked(op.handle)
		}
	}
}

// placeViewLocked 放置空间视野并维护跟随表：跟随的实体在场时视点取实体位置。
func (in *Interest) placeViewLocked(handle, follow int64, at spatial.Point, sight ObserverView) {
	if observer := in.aoi.observers[handle]; observer != nil && observer.self != follow {
		in.unfollowLocked(observer.self, handle)
	}
	if follow != 0 {
		if position, present := in.aoi.subjects[follow]; present {
			at = position
		}
		handles := in.followers[follow]
		if handles == nil {
			handles = make(map[int64]struct{})
			in.followers[follow] = handles
		}
		handles[handle] = struct{}{}
	}
	in.aoi.placeObserver(handle, follow, at, sight)
}

func (in *Interest) unfollowLocked(entityID, handle int64) {
	if handles := in.followers[entityID]; handles != nil {
		delete(handles, handle)
		if len(handles) == 0 {
			delete(in.followers, entityID)
		}
	}
}

// seedSpatialLocked 把空间视野当前的可见集给（enter）或从（!enter）一个玩家：挂接与解除的那一刻 AOI 事件已全部排空，
// 所以已生效的贡献恰好就是视野当前的可见集，解除时不会漏掉也不会多删。
func (in *Interest) seedSpatialLocked(playerID, handle int64, enter bool) {
	observer := in.aoi.observers[handle]
	if observer == nil {
		return
	}
	for _, subject := range sortedVisible(observer.visible) {
		in.contribute(pair{observer: playerID, subject: subject}, contribution{view: handle}, in.layerForBand(observer.visible[subject]), enter)
	}
	for _, subject := range sortedVisible(observer.blockVisible) {
		in.contribute(pair{observer: playerID, subject: subject}, contribution{view: handle, block: true}, in.blockLayer, enter)
	}
}

// removeSpatialLocked 撤掉一个空间视野：先从已生效的挂接玩家收回它的可见集，再撤出 AOI（之后它的离开事件找不到玩家）。
func (in *Interest) removeSpatialLocked(handle int64) {
	in.drainAOILocked()
	for _, playerID := range sortedIDs(in.spatialPlayers[handle]) {
		in.seedSpatialLocked(playerID, handle, false)
	}
	delete(in.spatialPlayers, handle)
	if observer := in.aoi.observers[handle]; observer != nil {
		in.unfollowLocked(observer.self, handle)
	}
	// 撤出后的离开事件找不到挂接玩家，drainAOILocked 会跳过它们；内部编号不复用。
	_ = in.aoi.RemoveObserver(handle)
}

// drainAOILocked 把 AOI 积攒的可见性变化按空间视野扇出给已生效的挂接玩家。
func (in *Interest) drainAOILocked() {
	events := in.aoi.Flush()
	for _, event := range events {
		in.routeSpatial(event, false)
	}
	// 仅内部 AOI 返回的本批事件由 Interest 独占；公开 Flush 的所有权不变。
	if len(events) > 0 && cap(events) <= 4096 {
		in.aoi.pending = events[:0]
	}
	for _, event := range in.aoi.FlushBlocks() {
		in.routeSpatial(event, true)
	}
}

func (in *Interest) routeSpatial(event InterestEvent, block bool) {
	players := in.spatialPlayers[event.Observer]
	if len(players) == 0 {
		return
	}
	enter := event.Kind != InterestLeave
	var layer entity.SyncProfile
	if enter {
		layer = in.blockLayer
		if !block {
			layer = in.layerForBand(event.Band)
		}
	}
	source := contribution{view: event.Observer, block: block}
	for playerID := range players {
		in.contribute(pair{observer: playerID, subject: event.Subject}, source, layer, enter)
	}
}

func (in *Interest) layerForBand(band int) entity.SyncProfile {
	return in.bandLayer(max(band, 0)).Normalize()
}

// ---- 层级合并 ----

// selectedProfile 是一个 pair 订阅的视图：各视野给的层级去重后，被别的层级包含（字段是其子集）的不再单独订阅——
// 包含关系取数据多的；字段相同的两个层级按 Manager 的优先级留一个；剩下的彼此不包含，取并集（entity.UnionSyncProfiles），
// packer 经 SyncViewSet.Lookup 拿到各层级字段的并集。不配置 ViewSets 时不知道字段，不同层级一律取并集。
func (in *Interest) selectedProfile(current *hold) entity.SyncProfile {
	var distinct [4]entity.SyncProfile
	layers := distinct[:0]
	for _, layer := range current.layers {
		if !slices.Contains(layers, layer) {
			layers = append(layers, layer)
		}
	}
	if len(layers) == 1 {
		return layers[0]
	}
	kept := make([]entity.SyncProfile, 0, len(layers))
	for i, candidate := range layers {
		covered := false
		for j, other := range layers {
			if i != j && in.layerCovers(other, candidate) && (!in.layerCovers(candidate, other) || in.compareProfiles(other, candidate) < 0) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, candidate)
		}
	}
	return entity.UnionSyncProfiles(kept...)
}

// layerCovers 报告层级 a 是否包含层级 b：配置的每个实体类型里 a 的字段都包含 b 的字段。没有 ViewSets 时不知道字段，返回 false。
func (in *Interest) layerCovers(a, b entity.SyncProfile) bool {
	if len(in.viewSets) == 0 {
		return false
	}
	key := [2]entity.SyncProfile{a, b}
	if covers, cached := in.covers[key]; cached {
		return covers
	}
	covers := true
	for _, views := range in.viewSets {
		if !views.Covers(a, b) {
			covers = false
			break
		}
	}
	in.covers[key] = covers
	return covers
}
