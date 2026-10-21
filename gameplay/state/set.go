package state

import (
	"fmt"
	"slices"
)

// maxNotifyRounds 限制一次最外层调用里的通知轮数：观察者改状态会引出下一轮，互相触发时截停。
const maxNotifyRounds = 64

// Change 是交给观察者的一次变化：状态 ID 与变化后的值。
type Change struct {
	ID     ID
	Active bool
}

// Handle 标识一次 Watch，用于 Unwatch。
type Handle uint64

// Requirement 是“能否做某个操作”的状态条件（对照 ssr 的 CheckOperation）：All 里的状态必须全部生效，None 里的必须
// 全部不生效。
type Requirement struct {
	All  []ID
	None []ID
}

// Set 是实体的状态组件：按来源的状态在 StateDao 里，派生状态由 Provide 登记的函数现算；Watch 的观察者在状态值变化
// 时被调用。方法只能在实体锁内调用。
//
// 通知的规则（D21 S4）：
//   - 观察者在实体锁内同步调用，顺序 = 注册顺序（不做优先级），只做内存计算与入队、不阻塞。
//   - 通知按“观察者上次看到的值”算差：每次改动（Add / Remove / Changed / Refresh）之后，对每个被观察的状态比较当前
//     值与上次交出的值，不同才通知。所以观察者看到的总是交替的 true / false，不会连收两次同一个值。
//   - 观察者里改状态立即生效，它引出的通知排到本轮之后（最外层调用返回前做完）；互相触发超过 64 轮截停，经
//     OnError 报告 ErrNotifyRunaway。
//   - 回滚不通知：事务回滚时 StateDao 回到事务开始的来源集合，观察者不会被调用（回滚期间不能再改别的组件）。观察者
//     的副作用（例如经 Gate 冻结了动作组）因此保留，直到下一次任何状态改动或 Refresh——那时按差补发，观察者看到的值
//     与实际重新一致。业务在 handler 回滚后需要立即一致时调 Refresh。
type Set struct {
	dao       *StateDao
	catalog   *Catalog
	providers map[ID]func() bool
	onError   func(error)

	watches []*watch
	nextH   Handle
	// seen 是每个被观察的状态上次交给观察者的值，用来按差通知。它是观察者视角的缓存，不随事务回滚（见类型说明）。
	seen map[ID]bool //roost:cache
	// notifying 为真表示正在通知（含观察者里的改动），改动只记下、由正在进行的通知循环补发。
	notifying bool //roost:cache
}

type watch struct {
	handle Handle
	id     ID
	fn     func(Change)
}

// SetConfig 是 NewSet 的可选项。
type SetConfig struct {
	// OnError 接收观察者 panic 与通知截停（ErrNotifyRunaway），可空。
	OnError func(error)
}

// NewSet 用实体的 StateDao 构造状态组件。
func NewSet(dao *StateDao, config SetConfig) *Set {
	return &Set{dao: dao, catalog: dao.catalog, providers: make(map[ID]func() bool), onError: config.OnError, seen: make(map[ID]bool)}
}

// Dao 返回持有按来源状态的 DAO。
func (s *Set) Dao() *StateDao { return s.dao }

// Catalog 返回状态声明表。
func (s *Set) Catalog() *Catalog { return s.catalog }

// Provide 登记派生状态的提供函数（构造时调用一次）：查询时现算、不存储；数据变化后由拥有数据的组件调 Changed。
// 状态必须已声明且没有声明 Persist，每个状态只能有一个提供函数。同一状态也可以同时有来源：任一为真即生效。
func (s *Set) Provide(id ID, fn func() bool) error {
	def, ok := s.catalog.Def(id)
	switch {
	case !ok:
		return fmt.Errorf("%w: %d", ErrUnknownState, id)
	case fn == nil:
		return fmt.Errorf("%w: nil provider for %q", ErrProviderInvalid, def.Name)
	case def.Persist:
		return fmt.Errorf("%w: %q is persistent, a derived state is never stored", ErrProviderInvalid, def.Name)
	}
	if _, dup := s.providers[id]; dup {
		return fmt.Errorf("%w: %q already has a provider", ErrProviderInvalid, def.Name)
	}
	s.providers[id] = fn
	return nil
}

// Add 给状态加一个来源（同一来源重复加不叠加）。声明了 Persist 的状态只能在 Nest 事务里改（ErrNotInTransaction）。
func (s *Set) Add(id ID, src Source) error { return s.change(id, src, true) }

// Remove 撤掉状态的一个来源（不存在的来源无效）。
func (s *Set) Remove(id ID, src Source) error { return s.change(id, src, false) }

// RemoveSource 从全部状态里撤掉一个来源（例如一个 Buff 实例结束、一个动作结束），返回被撤掉的状态。
func (s *Set) RemoveSource(src Source) ([]ID, error) {
	var removed []ID
	for _, id := range s.catalog.order {
		if !slices.Contains(s.dao.sources[id], src) {
			continue
		}
		if err := s.change(id, src, false); err != nil {
			return removed, err
		}
		removed = append(removed, id)
	}
	return removed, nil
}

func (s *Set) change(id ID, src Source, add bool) error {
	def, ok := s.catalog.Def(id)
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownState, id)
	}
	changed, err := s.dao.change(def, src, add)
	if err != nil || !changed {
		return err
	}
	s.notify()
	return nil
}

// Changed 由派生状态的数据拥有者在数据变化后调用：标同步脏（声明了 Sync 的），并按差通知观察者。不确定哪些状态受
// 影响时可以多传，没有变化的不会通知。
func (s *Set) Changed(ids ...ID) {
	for _, id := range ids {
		if def, ok := s.catalog.Def(id); ok && def.Sync {
			s.dao.tracker.MarkSync(FieldStates)
		}
	}
	s.notify()
}

// Refresh 按差补发全部被观察状态的通知（例如 handler 回滚之后要让观察者与状态立即一致）。
func (s *Set) Refresh() { s.notify() }

// Has 报告状态是否生效：有任一来源，或派生函数为真。未声明的状态为 false。
func (s *Set) Has(id ID) bool {
	if s.dao.has(id) {
		return true
	}
	if fn := s.providers[id]; fn != nil {
		return s.callProvider(id, fn)
	}
	return false
}

// Any 报告 ids 里是否有生效的状态。
func (s *Set) Any(ids ...ID) bool {
	return slices.ContainsFunc(ids, s.Has)
}

// Satisfies 报告条件是否满足：All 全部生效且 None 全部不生效。
func (s *Set) Satisfies(req Requirement) bool {
	for _, id := range req.All {
		if !s.Has(id) {
			return false
		}
	}
	return !s.Any(req.None...)
}

// Sources 返回状态当前的来源（副本，按 Kind、ID 升序）；派生的部分不算来源。
func (s *Set) Sources(id ID) []Source { return slices.Clone(s.dao.sources[id]) }

// Active 按编号升序返回全部生效的状态。
func (s *Set) Active() []ID {
	var out []ID
	for _, id := range s.catalog.order {
		if s.Has(id) {
			out = append(out, id)
		}
	}
	return out
}

// Synced 按编号升序返回生效且声明了 Sync 的状态：业务的同步打包读它（变化时 StateDao 的同步掩码 FieldStates 置位）。
func (s *Set) Synced() []ID {
	var out []ID
	for _, id := range s.catalog.order {
		if def := s.catalog.defs[id]; def.Sync && s.Has(id) {
			out = append(out, id)
		}
	}
	return out
}

// Watch 登记观察者：状态的值变化时以变化后的值调用 fn。登记时记下当前值作为“已看到”的起点，不立即调用。
func (s *Set) Watch(id ID, fn func(Change)) (Handle, error) {
	if _, ok := s.catalog.Def(id); !ok {
		return 0, fmt.Errorf("%w: %d", ErrUnknownState, id)
	}
	if fn == nil {
		return 0, fmt.Errorf("state: nil watcher for %d", id)
	}
	if _, watched := s.seen[id]; !watched {
		s.seen[id] = s.Has(id)
	}
	s.nextH++
	s.watches = append(s.watches, &watch{handle: s.nextH, id: id, fn: fn})
	return s.nextH, nil
}

// Unwatch 撤销一次 Watch；不存在的 handle 无效。
func (s *Set) Unwatch(h Handle) {
	index := slices.IndexFunc(s.watches, func(w *watch) bool { return w.handle == h })
	if index < 0 {
		return
	}
	id := s.watches[index].id
	s.watches = slices.Delete(s.watches, index, index+1)
	if !slices.ContainsFunc(s.watches, func(w *watch) bool { return w.id == id }) {
		delete(s.seen, id)
	}
}

// Watcher 把一组 Watch 打包，一次 Close 全部撤销（对照 ssr 的 state Agent），给生命周期与状态集合不同的使用方用。
type Watcher struct {
	set     *Set
	handles []Handle
}

// NewWatcher 返回空的观察者包。
func (s *Set) NewWatcher() *Watcher { return &Watcher{set: s} }

// Watch 同 Set.Watch，登记记进包里。
func (w *Watcher) Watch(id ID, fn func(Change)) error {
	h, err := w.set.Watch(id, fn)
	if err != nil {
		return err
	}
	w.handles = append(w.handles, h)
	return nil
}

// Close 撤销包里的全部登记，可重复调用。
func (w *Watcher) Close() {
	for _, h := range w.handles {
		w.set.Unwatch(h)
	}
	w.handles = nil
}

// notify 按差通知：比较每个被观察状态的当前值与上次交出的值，不同就记下新值、依次调用该状态的观察者。观察者里的
// 改动立即生效，它们引出的差在下一轮补发；正在通知时（观察者里的改动）只返回，由外层循环接着做。
func (s *Set) notify() {
	if s.notifying || len(s.watches) == 0 {
		return
	}
	s.notifying = true
	defer func() { s.notifying = false }()
	for round := 0; ; round++ {
		changes := s.diff()
		if len(changes) == 0 {
			return
		}
		if round >= maxNotifyRounds {
			s.report(fmt.Errorf("%w: %d rounds, still changing %v", ErrNotifyRunaway, maxNotifyRounds, changes))
			return
		}
		for _, change := range changes {
			// 快照：观察者里 Watch / Unwatch 不影响本次分发。
			for _, w := range slices.Clone(s.watches) {
				if w.id == change.ID {
					s.call(w, change)
				}
			}
		}
	}
}

// diff 按编号升序算出“当前值 ≠ 上次交出的值”的状态，并把它们记为已交出。
func (s *Set) diff() []Change {
	var changes []Change
	for _, id := range s.catalog.order {
		last, watched := s.seen[id]
		if !watched {
			continue
		}
		if now := s.Has(id); now != last {
			s.seen[id] = now
			changes = append(changes, Change{ID: id, Active: now})
		}
	}
	return changes
}

func (s *Set) call(w *watch, change Change) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.report(fmt.Errorf("state: watcher of %d panic: %v", change.ID, recovered))
		}
	}()
	w.fn(change)
}

func (s *Set) callProvider(id ID, fn func() bool) (active bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.report(fmt.Errorf("state: provider of %d panic: %v", id, recovered))
			active = false
		}
	}()
	return fn()
}

func (s *Set) report(err error) {
	if err != nil && s.onError != nil {
		defer func() { _ = recover() }()
		s.onError(err)
	}
}
