//go:build daoruntime

package testdata

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type persistPathCase struct {
	name       string
	mutate     func(*HeroDao)
	set, unset []string
}

func persistPathCases() []persistPathCase {
	return []persistPathCase{
		{"scalar_and_nested", func(d *HeroDao) { d.SetLevel(17); d.GetPos().SetX(91) }, []string{"level", "pos.x"}, nil},
		{"nested_and_scalar", func(d *HeroDao) { d.GetPos().SetX(91); d.SetLevel(17) }, []string{"level", "pos.x"}, nil},
		{"struct", func(d *HeroDao) { d.GetPos().SetX(91) }, []string{"pos.x"}, nil},
		{"nested_map", func(d *HeroDao) { e, _ := d.GetEquips(2); g, _ := e.GetGems(3); g.SetLevel(99) }, []string{"equips.2.gems.3.level"}, nil},
		{"list_index", func(d *HeroDao) { d.SetFriendsAt(50, 99) }, []string{"friends.50"}, nil},
		{"list_nested", func(d *HeroDao) { e, _ := d.GetSquad(50); e.SetStar(9) }, []string{"squad.50.star"}, nil},
		{"append", func(d *HeroDao) { d.AddFriends(99); d.AddFriends(100) }, []string{"friends.128", "friends.129"}, nil},
		{"map_remove", func(d *HeroDao) { d.DelItems(5) }, nil, []string{"items.5"}},
		{"nested_remove", func(d *HeroDao) { e, _ := d.GetEquips(2); e.DelGemsKey(3) }, nil, []string{"equips.2.gems.3"}},
		{"replace_then_child", func(d *HeroDao) { d.SetMount(&EquipInfo{level: 1}); d.GetMount().SetLevel(9) }, []string{"mount"}, nil},
		{"child_then_replace", func(d *HeroDao) { d.GetMount().SetLevel(9); d.SetMount(nil) }, []string{"mount"}, nil},
		{"delete_reinsert", func(d *HeroDao) {
			d.DelEquips(2)
			d.SetEquips(2, &EquipInfo{level: 4})
			e, _ := d.GetEquips(2)
			e.SetStar(7)
		}, []string{"equips.2"}, nil},
		{"replace_list_then_child", func(d *HeroDao) {
			d.SetSquadAll([]*EquipInfo{{level: 1}, {level: 2}})
			e, _ := d.GetSquad(1)
			e.SetLevel(5)
		}, []string{"squad"}, nil},
		{"nil_list_append", func(d *HeroDao) { d.SetFriendsAll(nil); d.AddFriends(9); d.AddFriends(10) }, []string{"friends"}, nil},
		{"dense", func(d *HeroDao) {
			for i := range 128 {
				d.SetItems(int64(i), 999)
				d.SetFriendsAt(i, 888)
			}
		}, []string{"friends", "items"}, nil},
	}
}

func capturePersist(t testing.TB, d *HeroDao, mutate func(*HeroDao)) nest.CommitRecord {
	t.Helper()
	c := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), c, "persist_paths", func() (any, error) { mutate(d); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if len(c.records) != 1 || len(c.records[0].Mutations) != 1 {
		t.Fatalf("records: %+v", c.records)
	}
	return c.records[0]
}
func TestPersistPathsReadFinalValues(t *testing.T) {
	for _, tc := range persistPathCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := collectionHero(128)
			d.tracker.SetVersion(1)
			rec := capturePersist(t, d, tc.mutate)
			m := rec.Mutations[0]
			if m.Kind != dataengine.MutationPatch {
				t.Fatal("expected patch")
			}
			if err := dataengine.ValidateMutation(m); err != nil {
				t.Fatal(err)
			}
			elements, err := bson.Raw(m.Patch.SetBSON).Elements()
			if len(m.Patch.SetBSON) > 0 && err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, e := range elements {
				keys = append(keys, e.Key())
			}
			slices.Sort(keys)
			slices.Sort(m.Patch.Unset)
			if !slices.Equal(keys, tc.set) || !slices.Equal(m.Patch.Unset, tc.unset) {
				t.Fatalf("set=%v unset=%v want %v / %v", keys, m.Patch.Unset, tc.set, tc.unset)
			}
			frozen := slices.Clone(m.Patch.SetBSON)
			capturePersist(t, d, func(d *HeroDao) { d.GetPos().SetY(101) })
			if !bytes.Equal(frozen, m.Patch.SetBSON) {
				t.Fatal("later write modified frozen WAL bytes")
			}
		})
	}
}
func TestPersistPathsRollbackAndSyncIndependence(t *testing.T) {
	d := collectionHero(128)
	d.tracker.SetVersion(1)
	before := d.Marshal()
	c := &ownershipCommitter{}
	refused := errors.New("refused")
	_, err := nest.RunIsolatedTransaction(context.Background(), c, "refuse", func() (any, error) { d.SetLevel(19); d.GetPos().SetX(99); d.SetFriendsAt(2, 100); return nil, refused })
	if !errors.Is(err, refused) || len(c.records) != 0 || !samePersistBSON(t, before, d.Marshal()) {
		t.Fatal("rollback leaked data or persistence")
	}
	rec := capturePersist(t, d, func(d *HeroDao) { d.GetPos().SetY(22); d.tracker.AcknowledgeSyncChanges(d.tracker.SyncChangeVersion()) })
	elems, _ := bson.Raw(rec.Mutations[0].Patch.SetBSON).Elements()
	if len(elems) != 1 || elems[0].Key() != "pos.y" {
		t.Fatal("Sync acknowledgement consumed persistence or rollback resurrected x")
	}
}

func TestPersistPathsUnsafeKeyAndBudgetFallback(t *testing.T) {
	v := NewVarietyDao()
	v.SetId(1)
	v.tracker.SetVersion(1)
	c := &ownershipCommitter{}
	_, err := nest.RunIsolatedTransaction(context.Background(), c, "unsafe", func() (any, error) { v.SetBlobs("safe", []byte{1}); v.SetBlobs("a.b", []byte{2}); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	var set bson.M
	if err := bson.Unmarshal(c.records[0].Mutations[0].Patch.SetBSON, &set); err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set["blobs"] == nil {
		t.Fatal("unsafe key did not replace whole map")
	}
	d := collectionHero(5000)
	d.tracker.SetVersion(1)
	r := capturePersist(t, d, func(d *HeroDao) {
		for i := range 4500 {
			d.SetItems(int64(i), 999)
		}
	})
	var dense bson.M
	if err := bson.Unmarshal(r.Mutations[0].Patch.SetBSON, &dense); err != nil {
		t.Fatal(err)
	}
	if len(dense) != 1 || dense["items"] == nil {
		t.Fatal("budget fallback lost map")
	}
}

func TestPersistPathsRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	for i, tc := range persistPathCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := collectionHero(128)
			d.SetId(int64(1000 + i))
			if _, err := env.write(t, d, true); err != nil {
				t.Fatal(err)
			}
			rec := capturePersist(t, d, tc.mutate)
			for j := range rec.Mutations {
				rec.Mutations[j].Key.Database = env.database
				rec.Mutations[j].Key.Scope = dataengine.DatabaseGlobal
			}
			rec = persistWALRoundtrip(t, rec)
			if err := env.store.Project(env.ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err := env.store.Project(env.ctx, rec); err != nil {
				t.Fatalf("idempotent replay: %v", err)
			}
			stored := env.stored(t, d.Id())
			delete(stored, "_version")
			delete(stored, "_last_tx")
			raw, err := bson.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistBSON(t, d.Marshal(), raw) {
				t.Fatal("Mongo document differs from final DAO state")
			}
			loaded := env.load(t, d.Id())
			if !bytes.Equal(d.MarshalSync(^uint64(0)), loaded.MarshalSync(^uint64(0))) {
				t.Fatal("cold-loaded state differs")
			}

		})
	}
}

func persistWALRoundtrip(t *testing.T, rec nest.CommitRecord) nest.CommitRecord {
	t.Helper()
	options := nestwal.DefaultOptions(t.TempDir())
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	var result nest.CommitRecord
	count := 0
	if err := reopened.Replay(context.Background(), func(_ nest.CommitFence, record nest.CommitRecord) error { result = record; count++; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range result.Mutations {
		if len(result.Mutations[i].Data) == 0 {
			result.Mutations[i].Data = nil
		}
		if len(result.Mutations[i].Patch.SetBSON) == 0 {
			result.Mutations[i].Patch.SetBSON = nil
		}
		if len(result.Mutations[i].Patch.Unset) == 0 {
			result.Mutations[i].Patch.Unset = nil
		}
	}
	if count != 1 || !reflect.DeepEqual(result.Mutations, rec.Mutations) {
		t.Fatalf("WAL reopen changed frozen mutation: before=%#v after=%#v", rec.Mutations, result.Mutations)
	}
	return result
}

func samePersistBSON(t testing.TB, a, b []byte) bool {
	t.Helper()
	decode := func(raw []byte) bson.M {
		var doc bson.M
		decoder := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
		decoder.DefaultDocumentM()
		if err := decoder.Decode(&doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

func TestPersistPathsNullContainersRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	d := NewHeroDao()
	d.SetId(2001)
	d.setEquipsRawMap(map[int64]*EquipInfo{2: {}})
	d.Init()
	if _, err := env.write(t, d, true); err != nil {
		t.Fatal(err)
	}
	// 冷加载会把 null map 水合为空容器，不能据此假定 Mongo 父路径已是文档。
	d = env.load(t, d.Id())
	rec := capturePersist(t, d, func(d *HeroDao) {
		d.AddFriends(9)
		d.AddFriends(10)
		d.SetItems(8, 88)
		e, _ := d.GetEquips(2)
		e.SetGemsKey(3, &GemInfo{level: 9})
	})
	rec.Mutations[0].Key.Database = env.database
	rec.Mutations[0].Key.Scope = dataengine.DatabaseGlobal
	rec = persistWALRoundtrip(t, rec)
	if err := env.store.Project(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	loaded := env.load(t, d.Id())
	if !bytes.Equal(d.MarshalSync(^uint64(0)), loaded.MarshalSync(^uint64(0))) {
		t.Fatal("null-container projection differs")
	}
}

func TestPersistPathsBinaryIsWholeValue(t *testing.T) {
	d := NewVarietyDao()
	d.SetId(9)
	d.resultJSON = []byte{1, 2, 3}
	d.DirtyTracker().SetVersion(1)
	c := &ownershipCommitter{}
	_, err := nest.RunIsolatedTransaction(context.Background(), c, "binary", func() (any, error) { d.SetResultJSONAt(1, 9); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	raw := bson.Raw(c.records[0].Mutations[0].Patch.SetBSON)
	value := raw.Lookup("result_json")
	_, bytesValue, ok := value.BinaryOK()
	if !ok || !bytes.Equal(bytesValue, []byte{1, 9, 3}) {
		t.Fatal("BSON binary must not use array index paths")
	}
}
