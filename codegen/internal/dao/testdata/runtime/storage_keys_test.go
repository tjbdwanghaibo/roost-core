//go:build daoruntime

package testdata

import (
	"bytes"
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func shortKeysFixture() *ShortKeysDao {
	d := NewShortKeysDao()
	d.SetId(1)
	d.experience = 10
	d.equipment.Set(7, &ShortItem{strength: 3, quantity: 4})
	d.equipment.Set(8, &ShortItem{strength: 5, quantity: 6})
	d.equipment.Set(9, &ShortItem{strength: 7, quantity: 8})
	d.followers = []*ShortItem{{strength: 7, quantity: 8}, {strength: 9, quantity: 10}}
	d.Init()
	return d
}

func mutateShortKeys(d *ShortKeysDao) {
	d.SetExperience(11)
	item, _ := d.GetEquipment(7)
	item.SetStrength(30)
	d.DelEquipment(8)
	follower, _ := d.GetFollowers(0)
	follower.SetQuantity(80)
}

func TestNumericStorageKeysRoundtrip(t *testing.T) {
	d := shortKeysFixture()
	d.tracker.SetVersion(1)
	c := &ownershipCommitter{}
	_, err := nest.RunIsolatedTransaction(context.Background(), c, "short_keys", func() (any, error) { mutateShortKeys(d); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	m := c.records[0].Mutations[0]
	if err := dataengine.ValidateMutation(m); err != nil {
		t.Fatal(err)
	}
	raw := bson.Raw(m.Patch.SetBSON)
	if raw.Lookup("1").Int64() != 11 || raw.Lookup("2.7.1").Int32() != 30 || raw.Lookup("3.0.2").Int32() != 80 || len(m.Patch.Unset) != 1 || m.Patch.Unset[0] != "2.8" {
		t.Fatalf("unexpected patch: %s / %v", raw, m.Patch.Unset)
	}
	restored := NewShortKeysDao()
	if err := restored.Unmarshal(d.Marshal()); err != nil {
		t.Fatal(err)
	}
	synced := NewShortKeysDao()
	if err := synced.ApplySync(d.MarshalSync(dataengine.AllFields)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.MarshalSync(dataengine.AllFields), synced.MarshalSync(dataengine.AllFields)) {
		t.Fatal("BSON/PB roundtrip differs")
	}
}

func TestNumericStorageKeysRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	d := shortKeysFixture()
	c := &ownershipCommitter{}
	for step, mutate := range []func(){func() { d.SetExperience(12) }, func() { mutateShortKeys(d) }} {
		c.records = nil
		if _, err := nest.RunIsolatedTransaction(env.ctx, c, "short_keys", func() (any, error) { mutate(); return nil, nil }); err != nil {
			t.Fatal(err)
		}
		rec := c.records[0]
		rec.Mutations[0].Key.Database = env.database
		rec.Mutations[0].Key.Scope = dataengine.DatabaseGlobal
		rec = persistWALRoundtrip(t, rec)
		for range 2 {
			if err := env.store.Project(env.ctx, rec); err != nil {
				t.Fatal(err)
			}
		}
		docs, err := env.store.Load(env.ctx, dataengine.LoadSpec{Database: env.database, Scope: dataengine.DatabaseGlobal, Resource: ShortKeysDaoCollection, Filter: map[string]any{"_id": d.Id()}})
		if err != nil || len(docs) != 1 {
			t.Fatalf("load: %v %v", docs, err)
		}
		restored := NewShortKeysDao()
		if err := restored.RestorePersisted(docs[0].Data, docs[0].Schema, docs[0].Version); err != nil {
			t.Fatal(err)
		}
		if restored.tracker.Version() != uint64(step+1) || !bytes.Equal(d.MarshalSync(dataengine.AllFields), restored.MarshalSync(dataengine.AllFields)) {
			t.Fatal("Mongo cold load differs")
		}
	}
}

func TestFullFieldDominatesManualPaths(t *testing.T) {
	for _, fullFirst := range []bool{true, false} {
		d := collectionHero(128)
		d.tracker.SetVersion(1)
		record := capturePersist(t, d, func(d *HeroDao) {
			full := func() { d.SetMount(&EquipInfo{level: 42}) }
			if fullFirst {
				full()
			}
			if err := nest.MarkPersistSet(d, heroDaoFieldMount, "mount.level", 99); err != nil {
				t.Fatal(err)
			}
			if err := nest.MarkPersistUnset(d, heroDaoFieldMount, "mount.star"); err != nil {
				t.Fatal(err)
			}
			if !fullFirst {
				full()
			}
		})
		m := record.Mutations[0]
		if err := dataengine.ValidateMutation(m); err != nil {
			t.Fatal(err)
		}
		if len(m.Patch.Unset) != 0 || bson.Raw(m.Patch.SetBSON).Lookup("mount", "level").Int32() != 42 {
			t.Fatalf("full replacement lost: %+v", m.Patch)
		}
	}
}

// 相同字段/值，仅 BSON 键不同；与标脏优化的前后对照分开测量。
func BenchmarkStorageKeyEncoding(b *testing.B) {
	short := shortKeysFixture()
	long := NewLongKeysDao()
	long.SetId(1)
	long.experience = short.experience
	long.equipment.Set(7, &LongItem{strength: 3, quantity: 4})
	long.equipment.Set(8, &LongItem{strength: 5, quantity: 6})
	long.equipment.Set(9, &LongItem{strength: 7, quantity: 8})
	long.followers = []*LongItem{{strength: 7, quantity: 8}, {strength: 9, quantity: 10}}
	for _, tc := range []struct {
		name   string
		encode func() []byte
	}{{"long", long.Marshal}, {"short", short.Marshal}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var raw []byte
			for b.Loop() {
				raw = tc.encode()
			}
			b.ReportMetric(float64(len(raw)), "bson-B/op")
		})
	}
}
