package testdata

import (
	"github.com/tjbdwanghaibo/cube/game/clientsync"
	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

var _ clientsync.SyncPacker

// Entity category constants
const (
	EntityCategoryPlayer entity.EntityCategory = 1
	EntityKindPlayer     entity.EntityKind     = 1
)

var _ = registerEntityKinds()

func registerEntityKinds() struct{} {
	entity.MustRegisterEntityKindCategory(EntityKindPlayer, EntityCategoryPlayer)
	return struct{}{}
}

// Component type constants
const (
	CompTypeBag    entity.ComponentType = 1
	CompTypeBattle entity.ComponentType = 2
)

// --- DAO ---

const (
	PlayerDaoDBName     = "game"
	PlayerDaoCollection = "players"
	MailDaoDBName       = "mail"
	MailDaoCollection   = "mails"
)

type PlayerDao struct {
	id      int64
	tracker dataengine.Tracker
	Name    string
	Level   int
}

func NewPlayerDao() *PlayerDao { return &PlayerDao{} }

type MailDao struct {
	id      int64
	tracker dataengine.Tracker
}

func NewMailDao() *MailDao { return &MailDao{} }

func (d *MailDao) Id() int64                         { return d.id }
func (d *MailDao) SetId(id int64)                    { d.id = id }
func (d *MailDao) DbName() string                    { return MailDaoDBName }
func (d *MailDao) CollName() string                  { return MailDaoCollection }
func (d *MailDao) Dirty() entity.IDirty              { return &d.tracker }
func (d *MailDao) CleanDirty()                       { d.tracker.SelfClean() }
func (d *MailDao) DirtyTracker() *dataengine.Tracker { return &d.tracker }
func (d *MailDao) Marshal() []byte                   { return nil }
func (d *MailDao) MarshalPersist(mask uint64) []byte { return nil }

func (d *PlayerDao) Id() int64                         { return d.id }
func (d *PlayerDao) SetId(id int64)                    { d.id = id }
func (d *PlayerDao) DbName() string                    { return PlayerDaoDBName }
func (d *PlayerDao) CollName() string                  { return PlayerDaoCollection }
func (d *PlayerDao) Dirty() entity.IDirty              { return &d.tracker }
func (d *PlayerDao) CleanDirty()                       { d.tracker.SelfClean() }
func (d *PlayerDao) DirtyTracker() *dataengine.Tracker { return &d.tracker }
func (d *PlayerDao) Marshal() []byte                   { return nil }
func (d *PlayerDao) MarshalPersist(mask uint64) []byte { return nil }

// --- Components (registered via global factory) ---

type BagComponent struct {
	entity.ComponentBase
	player *Player
}

func (b *BagComponent) Name() string { return "bag" }

type BattleComponent struct {
	entity.ComponentBase
	player *Player
}

func (b *BattleComponent) Name() string { return "battle" }

// Register component factories (normally in init())
func init() {
	entity.RegisterComponentFactory(CompTypeBag, func(owner any, param *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &BagComponent{player: owner.(*Player)}, nil
	})
	entity.RegisterComponentFactory(CompTypeBattle, func(owner any, param *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		return &BattleComponent{player: owner.(*Player)}, nil
	})
}

// --- Entity definition (手写) ---

//roost:entity entityKind=EntityKindPlayer sync=true syncTopic="player" subjectPacker=clientsync.PlayerSubjectPacker
type Player struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager

	bag    *BagComponent    `comp:"CompTypeBag"`
	battle *BattleComponent `comp:"CompTypeBattle"`
	dao    *PlayerDao       `dao:"players"`
	mail   *MailDao         `dao:"mails"`
}

const SyncTopicPlayer = "player"
