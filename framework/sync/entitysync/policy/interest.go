package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Interest 内建距离、额外块覆盖与自身来源；业务关系通过 Relation 添加。
const (
	SourceSpatial = "spatial"
	SourceSelf    = "self"
	SourceBlock   = "block"
)

// InterestConfig shapes an Interest.
type InterestConfig struct {
	// MaxQueuedFacts 限制正式提交事实队列；零值为 65536。
	MaxQueuedFacts int
	// BatchSize 是每批就绪事实的软上限，零值1024；不切开同一Nest提交。
	BatchSize int
	// Workers 限制同一批次并行观察者计算数；零值取GOMAXPROCS。
	Workers int
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// AOI 配置空间网格、进入/离开半径和距离分档；由 NewAOI 校验。
	AOI AOIConfig
	// Session names the session an observer's frames go to. Observers,
	// subjects and relations are keyed by ENTITY id — unique across kinds —
	// while the transport addresses sessions; this is the one translation.
	// Nil means the session id IS the observer id.
	Session func(observer int64) entitysync.SessionID
	// Profile turns a distance band into the profile a subscription asks
	// for. Nil means band 0 → the default profile, band n → {Key: "bandN", LOD: n}.
	Profile func(band int) entity.SyncProfile
	// SourceProfiles 显式覆盖某来源、某距离档的视图；未配置的档继续使用 Profile。
	// 例如 self→owner，spatial→near/far，team→team；构造时复制，来源名必须已声明。
	SourceProfiles map[string]map[int]entity.SyncProfile
	// ViewSets 非空时校验本 Interest 涉及的实体类型及其视图；构造时复制 map。
	// 每个类型应支持可选来源视图，动态关系 band 在订阅前校验。
	ViewSets map[string]*entity.SyncViewSet
	// Relations are the named relation sources to create ("team",
	// "friends", …); Relation(name) reaches them. SourceSelf is always there.
	Relations []string
	// SelfVisible 控制观察者是否通过 self 关系看见自己；空间来源不会建立自身订阅。
	// nil 或 true 启用，只有显式 false 才关闭。
	SelfVisible *bool
}

// Refusal is a subscribe the manager did not take. Interest keeps asking on
// every Apply until it is taken or the pair is released; the caller only
// decides how loudly to log it — Retry tells the first refusal from the ones
// after it.
type Refusal struct {
	Observer int64
	Subject  int64
	Retry    bool
	Err      error
}

// Interest is the area-of-interest policy: a spatial index plus any number of
// relation sources, aggregated into one subscription per (observer, subject)
// and said to the manager.
//
// Aggregation is the part worth reading. A pair can be held by more than one
// source at once — a teammate standing next to you is both spatial and team —
// so Interest retains each source: the FIRST source to claim a pair subscribes,
// and only the LAST source to drop it unsubscribes. Without that, a teammate
// walking out of view would take the team subscription with them.
//
// Interest.mu 串行化政策批次、生命周期和订阅归并；批次内按 observer 并行计算。
// Queue* 只持独立 queueMu，不等待空间计算。两个锁仅按 mu→queueMu 获取。
type Interest struct {
	// queueMu 只保护事实准入和角色登记；不能包住空间计算或订阅操作。
	queueMu          sync.Mutex
	queueClosed      bool
	roles            map[int64]interestRole
	inflight         int
	batchSize        int
	workers          int
	queueActive      bool
	facts            []queuedInterestFact
	maxQueuedFacts   int
	wake             func()
	cancelQueue      func()
	mu               sync.Mutex
	subscriptions    *entitysync.SubscriptionSource
	aoi              *AOI
	self             *RelationSource
	relations        map[string]*RelationSource
	sources          []Source
	session          func(int64) entitysync.SessionID
	profile          func(int) entity.SyncProfile
	sourceProfiles   map[string]map[int]entity.SyncProfile
	compareProfiles  func(entity.SyncProfile, entity.SyncProfile) int
	viewSets         map[string]*entity.SyncViewSet
	validatePriority func(entity.SyncView) error

	// held maps a pair to the sources currently claiming it and to whether
	// the manager has been told about it.
	held map[pair]*hold
	// retry holds the pairs whose subscribe the manager refused, waiting
	// for the next Apply to say them again.
	retry []pair
	// refused counts consecutive refusals per held pair; a pair at
	// retryStallAfter no longer holds Drain open (RR-20261006-49).
	refused map[pair]int
	closed  bool
}

type pair struct{ observer, subject int64 }

type hold struct {
	bands      map[string]int
	subscribed bool
	profile    entity.SyncProfile
}

func NewInterest(config InterestConfig) (*Interest, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	manager, err := NewAOI(config.AOI)
	if err != nil {
		return nil, err
	}
	session := config.Session
	if session == nil {
		session = func(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }
	}
	profile := config.Profile
	if profile == nil {
		profile = DefaultBandProfile
	}
	if config.BatchSize < 0 || config.Workers < 0 {
		return nil, ErrInterestConfig
	}
	in := &Interest{
		roles: make(map[int64]interestRole), batchSize: config.BatchSize, workers: config.Workers,
		aoi: manager, session: session, profile: profile,
		relations: make(map[string]*RelationSource), held: make(map[pair]*hold),
	}
	in.subscriptions = config.Manager.NewSubscriptionSourceWithResubmit(in.resubmit)
	in.compareProfiles = config.Manager.CompareProfiles
	in.validatePriority = config.Manager.ValidateViewPriority
	in.viewSets = make(map[string]*entity.SyncViewSet, len(config.ViewSets))
	for name, views := range config.ViewSets {
		if name == "" || views == nil {
			return nil, fmt.Errorf("policy: invalid view set %q", name)
		}
		in.viewSets[name] = views
	}
	in.sourceProfiles = make(map[string]map[int]entity.SyncProfile, len(config.SourceProfiles))
	for source, bands := range config.SourceProfiles {
		if source != SourceSpatial && source != SourceSelf && source != SourceBlock && !slices.Contains(config.Relations, source) {
			return nil, fmt.Errorf("policy: unknown profile source %q", source)
		}
		copy := make(map[int]entity.SyncProfile, len(bands))
		for band, profile := range bands {
			if band < 0 {
				return nil, fmt.Errorf("policy: invalid profile band %d", band)
			}
			copy[band] = profile.Normalize()
		}
		in.sourceProfiles[source] = copy
	}
	in.sources = []Source{aoiSource{manager: manager}, blockSource{manager: manager}}
	if config.SelfVisible == nil || *config.SelfVisible {
		in.self = NewRelationSource(SourceSelf)
		in.sources = append(in.sources, in.self)
	}
	for _, name := range config.Relations {
		if name == "" || name == SourceSpatial || name == SourceSelf || name == SourceBlock {
			return nil, fmt.Errorf("policy: relation name %q is reserved or empty", name)
		}
		if _, dup := in.relations[name]; dup {
			return nil, fmt.Errorf("policy: relation %q named twice", name)
		}
		relation := NewRelationSource(name)
		in.relations[name] = relation
		in.sources = append(in.sources, relation)
	}
	if err := in.validateConfiguredProfiles(config); err != nil {
		return nil, err
	}
	if config.MaxQueuedFacts < 0 {
		return nil, errors.New("policy: negative queued fact capacity")
	}
	in.maxQueuedFacts = config.MaxQueuedFacts
	if in.maxQueuedFacts == 0 {
		in.maxQueuedFacts = 65536
	}
	if in.batchSize == 0 {
		in.batchSize = 1024
	}
	in.wake = config.Manager.WakeSync
	in.cancelQueue = config.Manager.RegisterPolicy(in.applyQueued, in.queuedPending)
	return in, nil
}

// DefaultBandProfile is the profile mapping used when none is configured:
// band 0 (nearest) is the default profile, band n asks for LOD n.
func DefaultBandProfile(band int) entity.SyncProfile {
	if band <= 0 {
		return entity.SyncProfile{}
	}
	return entity.SyncProfile{Key: fmt.Sprintf("band%d", band), LOD: uint8(band)}
}

// aoiSource adapts the core manager to Source. Flush is called with
// Interest's lock held, which is why it takes none of its own.
type aoiSource struct{ manager *AOI }

func (aoiSource) Name() string             { return SourceSpatial }
func (s aoiSource) Flush() []InterestEvent { return s.manager.Flush() }

// Relation returns a configured relation source by name, or nil.
func (in *Interest) Relation(name string) *RelationSource {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.relations[name]
}

// Enter puts a player in: it both sees and is seen, and (when SelfVisible)
// it sees itself. Its subject must already be registered with the manager;
// its session must already be open.
func (in *Interest) Enter(id int64, at spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if err := in.aoi.AddSubject(id, at); err != nil {
		return err
	}
	if err := in.aoi.AddObserver(id, at); err != nil {
		return err
	}
	in.setRole(id, interestRole{subject: true, observer: true})
	if in.self != nil {
		in.self.Set(id, []int64{id})
	}
	return nil
}

// Move is for something that both sees and is seen. Both tables, and in this
// order: a subject that moved without its observer having moved would see the
// world from where it used to be.
func (in *Interest) Move(id int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if err := in.aoi.MoveSubject(id, to); err != nil {
		return err
	}
	return in.aoi.MoveObserver(id, to)
}

// Leave takes a player out in both directions and out of every relation.
func (in *Interest) Leave(id int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if in.self != nil {
		in.self.Clear(id)
	}
	for _, relation := range in.relations {
		relation.Clear(id)
		relation.Forget(id)
	}
	// 退出期间 Queue* 仍可入队；最后一次短队列锁统一作废退出前的关系事实。
	// 空间事实按实际移除的角色分别作废，不把 Hide 当成观察者也退出。
	defer in.supersedeQueuedRelationsLocked(id, true)
	if err := in.aoi.RemoveObserver(id); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(id, false, true)
	if err := in.aoi.RemoveSubject(id); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(id, true, false)
	return nil
}

// Show / MoveShown / Hide are for things that are seen but do not see: a
// monster, a dropped item.
func (in *Interest) Show(id int64, at spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if err := in.aoi.AddSubject(id, at); err != nil {
		return err
	}
	in.queueMu.Lock()
	r := in.roles[id]
	r.subject = true
	in.roles[id] = r
	in.queueMu.Unlock()
	return nil
}

func (in *Interest) MoveShown(id int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.aoi.MoveSubject(id, to)
}

func (in *Interest) Hide(id int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	for _, relation := range in.relations {
		relation.Forget(id)
	}
	defer in.supersedeQueuedRelationsLocked(id, false)
	if err := in.aoi.RemoveSubject(id); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(id, true, false)
	return nil
}

// Resubscribe says every pair this observer holds to the manager again on the
// next Apply, and reports how many it queued. It is for an observer whose
// session the manager dropped on its own — a transport failure it reported
// through ManagerConfig.SessionLost — while the observer stays in the policy
// (it still has a live connection) and its session has been opened again: the
// manager forgot the subscriptions, the policy did not, and without this the
// reopened session would receive nothing until the pairs changed. Pairs other
// observers hold on this subject are untouched; they never left.
//
// The re-said pairs go through the retry list, so a manager that still
// refuses one (the session is not open yet) is asked again on every Apply.
//
// 实体被框架退回 remove（RR-59 卸载后重载不了）之后重新登记的情形不需要调用它：Manager 把被撤销的订阅
// 交还 Interest，由 resubmit 自动重新提交（RR-20260926-70）。
func (in *Interest) Resubscribe(observer int64) int {
	if in == nil {
		return 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return 0
	}
	queued := 0
	for key, current := range in.held {
		if key.observer != observer || !current.subscribed {
			continue
		}
		current.subscribed = false
		in.retry = append(in.retry, key)
		queued++
	}
	return queued
}

// resubmit 是 Interest 的重新提交入口（RR-20260926-70）：RR-59 卸载后重载不了，框架退回 remove 撤销了本 Interest
// 的订阅，同 ID 实体又重新登记后，Manager 在 Flush 的政策阶段把这些订阅交还这里。与 Resubscribe 同一种修补：
// Manager 忘了，Interest 没忘。仍持有的 pair 按 Interest 自己的规则重新订阅（来源视图选择、ViewSets 校验、
// Manager 的优先级与字段白名单）；缺席期间已释放的 pair（观察者走远、关系解除、Hide）不在 held 里，不恢复。
// 被拒绝的 pair 进入重试表，下一次 Apply 以 Retry 报告并继续重说。
func (in *Interest) resubmit(retracted []entitysync.RetractedSubscription) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed || len(in.held) == 0 {
		return
	}
	wanted := make(map[int64][]entitysync.SessionID, len(retracted))
	for _, subscription := range retracted {
		wanted[subscription.Subject] = append(wanted[subscription.Subject], subscription.Session)
	}
	var refusals []Refusal // 已记入重试表，由下一次 Apply 报告
	for key, current := range in.held {
		sessions, retractedSubject := wanted[key.subject]
		if !retractedSubject || !current.subscribed || !slices.Contains(sessions, in.session(key.observer)) {
			continue
		}
		in.subscribe(key, current, true, &refusals)
	}
}

// Visible lists what an observer currently sees through distance.
func (in *Interest) Visible(observer int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	return in.aoi.Visible(observer)
}

// Apply drains every source, aggregates, and says the result to the manager:
// subscribes for pairs a first source claimed, unsubscribes for pairs the
// last source dropped, re-subscribes for pairs whose band changed. Subscribes
// the manager refuses come back as Refusals and are said again on the next
// Apply, for as long as the pair is held — a refusal is not a verdict on the
// pair (RR-20260920-06). Calling it is the caller's choice: Interest is a
// state machine, not a goroutine.
func (in *Interest) Apply() []Refusal {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.applyLocked()
}

func (in *Interest) applyLocked() []Refusal {
	if in.closed {
		return nil
	}
	var refusals []Refusal
	// Take the retry list first: a refusal during this Apply goes on the list
	// for the NEXT one, not for a second attempt a few lines further down.
	queued := in.retry
	in.retry = nil
	touched := make(map[pair]struct{})
	for _, source := range in.sources {
		name := source.Name()
		events := source.Flush()
		for _, event := range events {
			key := pair{observer: event.Observer, subject: event.Subject}
			touched[key] = struct{}{}
			in.recordEvent(name, event)
		}
		// 仅内部 AOI 返回的本批事件由 Interest 独占；公开 Flush 的所有权不变。
		if name == SourceSpatial && len(events) > 0 && cap(events) <= 4096 {
			in.aoi.pending = events[:0]
		}
	}
	keys := make([]pair, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b pair) int {
		if order := cmp.Compare(a.observer, b.observer); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})

	for _, key := range keys {
		in.settlePair(key, &refusals)
	}
	// Retries after the events, so that a pair a source event already spoke
	// for in this Apply is not said twice.
	for _, key := range queued {
		hold := in.held[key]
		if hold == nil || hold.subscribed {
			if hold == nil {
				delete(in.refused, key)
			}
			continue
		}
		in.subscribe(key, hold, true, &refusals)
	}
	return refusals
}

// 先收齐所有来源的最终状态，再向 Manager 发布；spatial 转为 block/team
// 可见时不能先退订后重建，否则会打断对象基线并产生闪烁。
func (in *Interest) recordEvent(source string, event InterestEvent) {
	key := pair{observer: event.Observer, subject: event.Subject}
	current := in.held[key]
	if event.Kind == InterestLeave {
		if current != nil {
			delete(current.bands, source)
		}
		return
	}
	if current == nil {
		current = &hold{bands: make(map[string]int, 2)}
		in.held[key] = current
	}
	current.bands[source] = max(event.Band, 0)
}
func (in *Interest) settlePair(key pair, refusals *[]Refusal) {
	current := in.held[key]
	if current == nil {
		return
	}
	if len(current.bands) == 0 {
		delete(in.held, key)
		delete(in.refused, key)
		err := in.subscriptions.Unsubscribe(in.session(key.observer), key.subject)
		if err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			*refusals = append(*refusals, Refusal{Observer: key.observer, Subject: key.subject, Err: err})
		}
		return
	}
	if !current.subscribed {
		in.subscribe(key, current, false, refusals)
	} else {
		in.reband(key, current, refusals)
	}
}

// subscribe says one pair to the manager. The pair counts as subscribed from
// here — that is what makes the source counting work — and a refusal puts it
// on the retry list instead.
func (in *Interest) subscribe(key pair, current *hold, retry bool, refusals *[]Refusal) {
	current.profile = in.selectedProfile(current)
	current.subscribed = true
	err := in.validateProfile(current.profile)
	if err == nil {
		err = in.subscriptions.Subscribe(in.session(key.observer), key.subject, current.profile)
	}
	if err == nil {
		delete(in.refused, key)
		return
	}
	current.subscribed = false
	in.retry = append(in.retry, key)
	in.noteRefusedLocked(key, err)
	*refusals = append(*refusals, Refusal{Observer: key.observer, Subject: key.subject, Retry: retry, Err: err})
}

func (in *Interest) reband(key pair, current *hold, refusals *[]Refusal) {
	if !current.subscribed || in.selectedProfile(current) == current.profile {
		return
	}
	in.subscribe(key, current, false, refusals)
}

// selectedProfile 先解析每个来源的业务视图，再用 Manager 的同一规则选优。
// 不先折叠成最小 band，避免丢失 self/team 等来源的字段语义。
func (in *Interest) selectedProfile(current *hold) entity.SyncProfile {
	var best entity.SyncProfile
	first := true
	for source, band := range current.bands {
		profile := in.profileFor(source, band)
		if first || in.compareProfiles(profile, best) < 0 {
			best, first = profile, false
		}
	}
	return best
}

// Close 释放本 Interest 来源持有的订阅；其他政策、会话和实体注册继续存在。
func (in *Interest) Close() {
	if in == nil {
		return
	}
	if in.cancelQueue != nil {
		in.cancelQueue()
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.closed = true
	in.queueMu.Lock()
	in.queueClosed = true
	for _, fact := range in.facts {
		fact.finish()
	}
	in.facts = nil
	clear(in.roles)
	in.queueMu.Unlock()
	for key := range in.held {
		_ = in.subscriptions.Unsubscribe(in.session(key.observer), key.subject)
	}
	in.held = nil
	in.retry = nil
	in.refused = nil
}
