package level

import (
	"bytes"
	"encoding/json"
	"io"
	"math"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const CollectionName = "level_state"
const schemaVersion uint32 = 1

// LevelDAO 首版把有界运行状态作为一个字段持久化；不把运行指针或 Program 放进存储。
// State 用 JSON 字节封装进 BSON，保留 uint64 Epoch 全范围；不是逐字段同步协议。
type LevelDAO struct {
	persistent bool
	id         int64
	db         string
	tracker    dataengine.Tracker
	state      State
}

func NewLevelDAO(id int64, db string) *LevelDAO { return &LevelDAO{id: id, db: db, persistent: true} }

// NewTransientLevelDAO 用于不落库的瞬态 Room；仍参与 Nest 回滚。
func NewTransientLevelDAO(id int64) *LevelDAO { return &LevelDAO{id: id} }

func (d *LevelDAO) Id() int64                         { return d.id }
func (d *LevelDAO) SetId(id int64)                    { d.id = id }
func (d *LevelDAO) DbName() string                    { return d.db }
func (d *LevelDAO) CollName() string                  { return CollectionName }
func (d *LevelDAO) Dirty() entity.IDirty              { return &d.tracker }
func (d *LevelDAO) CleanDirty()                       { d.tracker.SelfClean() }
func (d *LevelDAO) DirtyTracker() *dataengine.Tracker { return &d.tracker }
func (d *LevelDAO) SchemaVersion() uint32             { return schemaVersion }
func (d *LevelDAO) replace(state State) error {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return nest.ErrTransactionClosed
	}
	if tx.Policy() == nest.RollbackUndo {
		before := d.state
		if !nest.RecordUndo(d, 1, func() error { d.state = before; return nil }) {
			return nest.ErrTransactionClosed
		}
	} else if tx.Policy() != nest.RollbackState {
		return ErrState
	}
	if d.persistent {
		if err := nest.MarkPersist(d, 1); err != nil {
			return err
		}
	}
	d.state = state
	d.tracker.MarkSync(1)
	return nil
}
func (d *LevelDAO) CaptureRollbackState() ([]byte, error) { return json.Marshal(d.state) }
func (d *LevelDAO) RestoreRollbackState(raw []byte) error {
	s, err := decodeState(raw)
	if err != nil {
		return err
	}
	d.state = s
	return nil
}
func decodeState(raw []byte) (State, error) {
	var s State
	if len(raw) > 2<<20 {
		return s, ErrBudget
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return s, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return s, ErrState
	}
	return s, s.validate()
}

type persisted struct {
	ID    int64  `bson:"_id"`
	State []byte `bson:"state"`
}

func (d *LevelDAO) MarshalPersisted() ([]byte, uint32, error) {
	raw, err := json.Marshal(d.state)
	if err != nil {
		return nil, 0, err
	}
	data, err := bson.Marshal(persisted{d.id, raw})
	return data, schemaVersion, err
}
func (d *LevelDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if schema != schemaVersion || len(raw) > 3<<20 {
		return ErrState
	}
	var doc persisted
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.ID != d.id {
		return ErrState
	}
	state, err := decodeState(doc.State)
	if err != nil {
		return err
	}
	d.state = state
	d.tracker.SetVersion(version)
	d.tracker.SelfClean()
	return nil
}
func (d *LevelDAO) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	if !d.persistent {
		return dataengine.Mutation{}, ErrState
	}
	version := d.tracker.Version()
	if version == math.MaxUint64 {
		return dataengine.Mutation{}, ErrBudget
	}
	m := dataengine.Mutation{Key: dataengine.DocumentKey{Database: d.db, Resource: CollectionName, ID: d.id}, Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1, Schema: schemaVersion, Codec: "bson-v2", Mask: change.Mask}
	if change.Delete {
		m.Kind = dataengine.MutationDelete
		return m, nil
	}
	raw, _, err := d.MarshalPersisted()
	m.Data = raw
	return m, err
}
func (d *LevelDAO) AcceptMutation(m dataengine.Mutation) error {
	return d.tracker.AcceptVersion(m.ExpectedVersion, m.NextVersion)
}
