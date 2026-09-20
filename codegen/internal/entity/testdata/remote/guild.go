package remote

import (
	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

const (
	EntityKindGuild    entity.EntityKind = 201
	GuildDAOCollection                   = "guilds"
)

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: EntityKindGuild, Category: 6, RemotePolicy: entity.RemotePolicyManaged})
}

type GuildDAO struct {
	id      int64
	tracker dataengine.Tracker
	Name    string
}

func NewGuildDAO() *GuildDAO                          { return &GuildDAO{} }
func (d *GuildDAO) Id() int64                         { return d.id }
func (d *GuildDAO) SetId(id int64)                    { d.id = id }
func (d *GuildDAO) DbName() string                    { return "game" }
func (d *GuildDAO) CollName() string                  { return GuildDAOCollection }
func (d *GuildDAO) Dirty() entity.IDirty              { return &d.tracker }
func (d *GuildDAO) CleanDirty()                       { d.tracker.SelfClean() }
func (d *GuildDAO) DirtyTracker() *dataengine.Tracker { return &d.tracker }
func (d *GuildDAO) MarshalPersist(uint64) []byte      { return []byte(d.Name) }
func (d *GuildDAO) ApplySync(data []byte) error       { d.Name = string(data); return nil }
func (d *GuildDAO) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := d.tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: d.DbName(), Resource: d.CollName(), ID: d.Id()},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Data: d.MarshalPersist(change.Mask),
	}, nil
}
func (d *GuildDAO) AcceptMutation(mutation dataengine.Mutation) error {
	return d.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

//roost:entity entityKind=EntityKindGuild remote=managed
type Guild struct {
	*entity.RemoteEntityBase
	entity.DaoManager

	dao *GuildDAO `dao:"guilds"`
}
