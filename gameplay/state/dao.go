package state

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// CollectionName 是状态的缺省持久集合。
const CollectionName = "entity_state"

// stateSchemaVersion 是持久文档的版本；不认识更新的版本（线上未部署，不做旧格式迁移）。
const stateSchemaVersion uint32 = 1

// 字段位（持久掩码与同步掩码共用一个位：状态集合整体是一个字段）。
const (
	// FieldStates 是“按来源记下的状态”：声明了 Persist 的状态变化时进持久掩码，声明了 Sync 的变化（含派生状态的
	// Changed）时进同步掩码。
	FieldStates uint64 = 1 << 0
)

// StateDao 持有一个实体按来源记下的状态（派生状态不存储），是手写 DAO，形状与生成 DAO 相同：实现
// entity.DaoInterface、nest 的持久与回滚契约与 entity.PersistedDaoLoader。
//
// 回滚（维护者 2026-10-21 决定 10：状态是数据，随事务回滚）：rollback=undo 下第一次改动前登记一次逆操作，恢复到事务
// 开始时的来源集合；rollback=state 下由 Nest 的 DAO 快照（CaptureRollbackState）恢复，这时 DAO 必须登记在实体的
// DaoManager 里。持久：只有声明了 Persist 的状态进持久文档，文档键是状态名（{_id, states: {名字: [{k, i}…]}}）；
// 加载时不认识的名字与不再声明 Persist 的状态被跳过，下一次整文档写回时消失（与 D13 对改名字段的处理一致）。
// 没有任何 Persist 状态时不必把 DAO 交给持久层（不登记进 DaoManager 也能回滚与同步）。
type StateDao struct {
	id      int64
	dbName  string
	coll    string
	tracker dataengine.Tracker
	catalog *Catalog

	// sources 是每个状态的来源，按 (Kind, ID) 升序、不重复；没有来源的状态不出现。
	sources map[ID][]Source
}

// NewStateDao 为实体的存储 ID 构造空 DAO。dbName 是存储服务解析的逻辑库名。
func NewStateDao(id int64, dbName string, catalog *Catalog) *StateDao {
	return &StateDao{id: id, dbName: dbName, coll: CollectionName, catalog: catalog, sources: make(map[ID][]Source)}
}

// 以下访问器实现 entity.DaoInterface 与 nest 脏标记契约。
func (dao *StateDao) Id() int64                         { return dao.id }
func (dao *StateDao) SetId(id int64)                    { dao.id = id }
func (dao *StateDao) DbName() string                    { return dao.dbName }
func (dao *StateDao) CollName() string                  { return dao.coll }
func (dao *StateDao) Dirty() entity.IDirty              { return &dao.tracker }
func (dao *StateDao) CleanDirty()                       { dao.tracker.SelfClean() }
func (dao *StateDao) DirtyTracker() *dataengine.Tracker { return &dao.tracker }

// SchemaVersion 返回持久文档的当前版本。
func (*StateDao) SchemaVersion() uint32 { return stateSchemaVersion }

// Catalog 返回状态声明表。
func (dao *StateDao) Catalog() *Catalog { return dao.catalog }

type persistedSource struct {
	Kind uint8 `bson:"k" json:"k"`
	ID   int64 `bson:"i" json:"i"`
}

type persistedStates struct {
	ID     int64                        `bson:"_id" json:"-"`
	States map[string][]persistedSource `bson:"states" json:"states"`
}

// persisted 只取声明了 Persist 的状态；rollback 快照用 all=true 取全部。
func (dao *StateDao) snapshot(all bool) persistedStates {
	out := persistedStates{ID: dao.id, States: make(map[string][]persistedSource)}
	for id, sources := range dao.sources {
		def, ok := dao.catalog.Def(id)
		if !ok || (!all && !def.Persist) {
			continue
		}
		list := make([]persistedSource, len(sources))
		for i, src := range sources {
			list[i] = persistedSource{Kind: src.Kind, ID: src.ID}
		}
		out.States[def.Name] = list
	}
	return out
}

// apply 用文档替换来源集合；onlyPersist 时跳过不再声明 Persist 的状态。不认识的名字一律跳过。
func (dao *StateDao) apply(doc persistedStates, onlyPersist bool) {
	sources := make(map[ID][]Source, len(doc.States))
	for name, list := range doc.States {
		id, ok := dao.catalog.Lookup(name)
		if !ok || len(list) == 0 {
			continue
		}
		if def, _ := dao.catalog.Def(id); onlyPersist && !def.Persist {
			continue
		}
		set := make([]Source, 0, len(list))
		for _, src := range list {
			set = insertSource(set, Source{Kind: src.Kind, ID: src.ID})
		}
		sources[id] = set
	}
	dao.sources = sources
}

// MarshalPersisted 序列化声明了 Persist 的状态。
func (dao *StateDao) MarshalPersisted() ([]byte, uint32, error) {
	payload, err := bson.Marshal(dao.snapshot(false))
	if err != nil {
		return nil, 0, err
	}
	return payload, stateSchemaVersion, nil
}

// RestorePersisted 实现 entity.PersistedDaoLoader：只恢复仍声明 Persist 的状态。
func (dao *StateDao) RestorePersisted(raw []byte, schemaVersion uint32, version uint64) error {
	if schemaVersion != stateSchemaVersion {
		return fmt.Errorf("state: persisted schema version %d, want %d", schemaVersion, stateSchemaVersion)
	}
	var doc persistedStates
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("state: decode persisted BSON: %w", err)
	}
	if doc.ID != 0 && doc.ID != dao.id {
		return fmt.Errorf("state: persisted id %d does not match dao id %d", doc.ID, dao.id)
	}
	dao.apply(doc, true)
	dao.tracker.SetVersion(version)
	dao.tracker.SelfClean()
	return nil
}

// PrepareMutation 生成带期望版本的写入：首次写或全字段时整文档 Put，否则 $set states。
func (dao *StateDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := dao.tracker.Version()
	mutation := dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: dao.DbName(), Resource: dao.CollName(), ID: dao.Id()},
		Kind: dataengine.MutationPatch, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: stateSchemaVersion, Codec: "bson-v2",
	}
	if change.Delete {
		mutation.Kind = dataengine.MutationDelete
		return mutation, nil
	}
	if version == 0 || change.Mask == dataengine.AllFields {
		payload, _, err := dao.MarshalPersisted()
		if err != nil {
			return dataengine.Mutation{}, err
		}
		mutation.Kind = dataengine.MutationPut
		mutation.Data = payload
		return mutation, nil
	}
	if change.Mask&FieldStates == 0 {
		return dataengine.Mutation{}, fmt.Errorf("state: persistence mask %#x has no fields", change.Mask)
	}
	patch, err := bson.Marshal(bson.M{"states": dao.snapshot(false).States})
	if err != nil {
		return dataengine.Mutation{}, err
	}
	mutation.Patch = dataengine.FieldPatch{SetBSON: patch}
	return mutation, nil
}

// AcceptMutation 在写入被准入后推进版本。
func (dao *StateDao) AcceptMutation(mutation dataengine.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

// CaptureRollbackState 与 RestoreRollbackState 实现 rollback=state 的快照（含不持久的状态）。
func (dao *StateDao) CaptureRollbackState() ([]byte, error) { return json.Marshal(dao.snapshot(true)) }

func (dao *StateDao) RestoreRollbackState(raw []byte) error {
	var doc persistedStates
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("state: decode rollback snapshot: %w", err)
	}
	dao.apply(doc, false)
	return nil
}

// has 报告状态是否有来源。
func (dao *StateDao) has(id ID) bool { return len(dao.sources[id]) > 0 }

// change 改一个状态的来源集合：add 为真时加入 src，否则移除。没有变化时什么都不做、返回 false。事务里第一次改动前
// 登记逆操作（rollback=undo），改完标脏：声明 Persist 的进持久掩码，声明 Sync 的进同步掩码。事务外只允许改不持久的
// 状态（加载 / 新建的初始化阶段），不登记、不标持久。
func (dao *StateDao) change(def Def, src Source, add bool) (bool, error) {
	current := dao.sources[def.ID]
	index, found := slices.BinarySearchFunc(current, src, compareSource)
	if add == found {
		return false, nil
	}
	tx := nest.CurrentRollbackTx()
	if tx == nil && def.Persist {
		return false, fmt.Errorf("%w: %q", ErrNotInTransaction, def.Name)
	}
	if tx != nil && tx.Policy() == nest.RollbackUndo && !tx.HasUndo(dao, FieldStates) {
		before := cloneSources(dao.sources)
		nest.RecordUndo(dao, FieldStates, func() error {
			dao.sources = before
			return nil
		})
	}
	next := slices.Clone(current)
	if add {
		next = slices.Insert(next, index, src)
	} else {
		next = slices.Delete(next, index, index+1)
	}
	if len(next) == 0 {
		delete(dao.sources, def.ID)
	} else {
		dao.sources[def.ID] = next
	}
	if tx != nil && def.Persist {
		if err := nest.MarkPersist(dao, FieldStates); err != nil {
			panic(fmt.Errorf("state: mark persistence: %w", err))
		}
	}
	if def.Sync {
		dao.tracker.MarkSync(FieldStates)
	}
	return true, nil
}

func compareSource(a, b Source) int {
	switch {
	case a.less(b):
		return -1
	case b.less(a):
		return 1
	default:
		return 0
	}
}

func insertSource(list []Source, src Source) []Source {
	index, found := slices.BinarySearchFunc(list, src, compareSource)
	if found {
		return list
	}
	return slices.Insert(list, index, src)
}

// cloneSources 复制来源集合：切片从不原地修改（change 总是换新切片），浅拷贝 map 即可得到独立的“修改前”。
func cloneSources(sources map[ID][]Source) map[ID][]Source { return maps.Clone(sources) }
