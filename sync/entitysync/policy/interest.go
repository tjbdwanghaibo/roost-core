package policy

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// SourceSpatial and SourceSelf are the two sources every Interest carries:
// distance, and "you always see yourself". A game adds its relationships by
// name through Relation.
const (
	SourceSpatial = "spatial"
	SourceSelf    = "self"
)

// InterestConfig shapes an Interest.
type InterestConfig struct {
	// MaxQueuedFacts 限制正式提交事实队列；零值为 65536。
	MaxQueuedFacts int
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
	// SelfVisible: an entering observer sees itself (through the self
	// relation, because the spatial source never lets anything observe
	// itself). On by default; set to false only through NewInterest's
	// zero-value semantics being explicit — see below.
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
// AOI is not safe for concurrent use; Interest carries the
// lock that makes it so. Every method is callable from any goroutine.
type Interest struct {
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
	retry  []pair
	closed bool
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
	in := &Interest{
		subscriptions: config.Manager.NewSubscriptionSource(), aoi: manager, session: session, profile: profile,
		relations: make(map[string]*RelationSource), held: make(map[pair]*hold),
	}
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
		if source != SourceSpatial && source != SourceSelf && !slices.Contains(config.Relations, source) {
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
	in.sources = []Source{aoiSource{manager: manager}}
	if config.SelfVisible == nil || *config.SelfVisible {
		in.self = NewRelationSource(SourceSelf)
		in.sources = append(in.sources, in.self)
	}
	for _, name := range config.Relations {
		if name == "" || name == SourceSpatial || name == SourceSelf {
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
	if err := in.aoi.RemoveObserver(id); err != nil {
		return err
	}
	return in.aoi.RemoveSubject(id)
}

// Show / MoveShown / Hide are for things that are seen but do not see: a
// monster, a dropped item.
func (in *Interest) Show(id int64, at spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.aoi.AddSubject(id, at)
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
	return in.aoi.RemoveSubject(id)
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
	if in.closed {
		return nil
	}
	var refusals []Refusal
	// Take the retry list first: a refusal during this Apply goes on the list
	// for the NEXT one, not for a second attempt a few lines further down.
	queued := in.retry
	in.retry = nil
	for _, source := range in.sources {
		name := source.Name()
		events := source.Flush()
		for _, event := range events {
			in.applyEvent(name, event, &refusals)
		}
		// 仅内部 AOI 返回的本批事件由 Interest 独占；公开 Flush 的所有权不变。
		if name == SourceSpatial && len(events) > 0 && cap(events) <= 4096 {
			in.aoi.pending = events[:0]
		}
	}
	// Retries after the events, so that a pair a source event already spoke
	// for in this Apply is not said twice.
	for _, key := range queued {
		hold := in.held[key]
		if hold == nil || hold.subscribed {
			continue
		}
		in.subscribe(key, hold, true, &refusals)
	}
	return refusals
}

// applyEvent folds one source's event into the pair's held set and says the
// change it caused, if any.
func (in *Interest) applyEvent(source string, event InterestEvent, refusals *[]Refusal) {
	key := pair{observer: event.Observer, subject: event.Subject}
	current := in.held[key]
	switch event.Kind {
	case InterestLeave:
		if current == nil {
			return
		}
		delete(current.bands, source)
		if len(current.bands) > 0 {
			// 其他来源仍持有；重新选择其视图，必要时触发全量恢复。
			in.reband(key, current, refusals)
			return
		}
		delete(in.held, key)
		err := in.subscriptions.Unsubscribe(in.session(key.observer), key.subject)
		if err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			*refusals = append(*refusals, Refusal{Observer: key.observer, Subject: key.subject, Err: err})
		}
	default:
		band := max(event.Band, 0)
		if current == nil {
			current = &hold{bands: make(map[string]int, 2)}
			in.held[key] = current
		}
		current.bands[source] = band
		if !current.subscribed {
			in.subscribe(key, current, false, refusals)
			return
		}
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
		return
	}
	current.subscribed = false
	in.retry = append(in.retry, key)
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
	in.facts = nil
	for key := range in.held {
		_ = in.subscriptions.Unsubscribe(in.session(key.observer), key.subject)
	}
	in.held = nil
	in.retry = nil
}
