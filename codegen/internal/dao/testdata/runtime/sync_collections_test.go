//go:build daoruntime

package testdata

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func collectionHero(size int) *HeroDao {
	d := newHero()
	d.SetId(42)
	items := make(map[int64]int32, size)
	d.friends = make([]int64, size)
	d.squad = make([]*EquipInfo, size)
	for i := range size {
		items[int64(i)] = int32(i + 1)
		d.friends[i] = int64(i + 1000)
		d.squad[i] = &EquipInfo{star: 1}
	}
	d.mount = &EquipInfo{level: 42}
	d.setItemsRawMap(items)
	d.Init()
	return d
}
func syncMutation(t testing.TB, fn func() error) error {
	t.Helper()
	_, err := nest.RunIsolatedTransaction(context.Background(), &ownershipCommitter{}, "sync", func() (any, error) { return nil, fn() })
	return err
}

func TestTrackedSyncChanges(t *testing.T) {
	type golden struct {
		Name  string `json:"name"`
		Full  string `json:"full"`
		Patch string `json:"patch"`
		Want  string `json:"want"`
	}
	var rows []golden
	for _, tc := range []struct {
		name   string
		mutate func(*HeroDao)
	}{
		{"sparse", func(d *HeroDao) { d.SetItems(5, 999); d.DelItems(7); d.AddFriends(7777) }},
		{"nested_nil", func(d *HeroDao) { d.SetMount(nil); d.SetEquips(2, nil); d.SetSquadAt(50, nil) }},
		{"nested_value", func(d *HeroDao) { item, _ := d.GetEquips(2); item.GetShape().SetX(99) }},
		{"nested_struct", func(d *HeroDao) { d.GetPos().SetX(0); d.GetMount().SetLevel(9) }},
		{"nested_map", func(d *HeroDao) { item, _ := d.GetEquips(2); gem, _ := item.GetGems(3); gem.SetLevel(999) }},
		{"indexed", func(d *HeroDao) {
			d.SetFriendsAt(50, 777)
			d.SetSquadAt(60, &EquipInfo{star: 6})
			item, _ := d.GetEquips(2)
			item.SetGemsKey(4, &GemInfo{level: 88})
			item.DelGemsKey(3)
		}},
		{"nested_list", func(d *HeroDao) { item, _ := d.GetSquad(50); item.SetStar(9) }},
		{"clear", func(d *HeroDao) {
			for i := int64(0); i < 128; i++ {
				d.DelItems(i)
			}
			d.SetFriendsAll(nil)
		}},
		{"dense", func(d *HeroDao) {
			for i := int64(0); i < 128; i++ {
				d.SetItems(i, 999)
			}
			d.SetFriendsAll([]int64{9, 8, 7})
		}},
		{"replace_then_child", func(d *HeroDao) { d.SetMount(&EquipInfo{level: 2}); d.GetMount().SetStar(7) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := collectionHero(128)
			old := d.MarshalSync(^uint64(0))
			if err := syncMutation(t, func() error { tc.mutate(d); return nil }); err != nil {
				t.Fatal(err)
			}
			delta := d.MarshalSyncChanges(d.DirtyTracker().SyncDirtyMask())
			want := d.MarshalSync(^uint64(0))
			patch, err := UnmarshalSyncHeroDao(delta)
			if err != nil {
				t.Fatal(err)
			}
			previous, _ := UnmarshalSyncHeroDao(old)
			next, err := ApplySyncHeroDao(patch, previous)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := MarshalSyncHeroDao(next)
			if !bytes.Equal(got, want) {
				t.Fatal("patch did not reconstruct current state")
			}
			original, _ := MarshalSyncHeroDao(previous)
			if !bytes.Equal(old, original) {
				t.Fatal("baseline mutated")
			}
			mirror := NewHeroDao()
			if err := mirror.ApplySync(old); err != nil {
				t.Fatal(err)
			}
			if err := mirror.ApplySync(delta); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(mirror.MarshalSync(^uint64(0)), want) {
				t.Fatal("DAO apply mismatch")
			}
			if tc.name == "nested_map" {
				child := patch.Equips.Patches[2].Gems.Patches[3]
				if child.Level == nil || child.Level.Value != 999 || child.ID != nil {
					t.Fatal("nested map field not sparse")
				}
			}
			if tc.name == "nested_list" {
				if patch.Squad.Patches[50].Star.Value != 9 || len(patch.Squad.Updates) != 0 {
					t.Fatal("nested list not sparse")
				}
			}
			if tc.name == "dense" && (patch.Items.Delta || patch.Friends.Delta) {
				t.Fatal("dense fallback missing")
			}
			rows = append(rows, golden{tc.name, hex.EncodeToString(old), hex.EncodeToString(delta), hex.EncodeToString(want)})
			t.Logf("full=%d patch=%d", len(want), len(delta))
		})
	}
	if base := os.Getenv("ROOST_DAO_SYNC_GOLDEN"); base != "" {
		path := filepath.Join(filepath.Dir(base), "dao-collections.json")
		raw, _ := json.MarshalIndent(rows, "", "  ")
		raw = append(raw, '\n')
		if os.Getenv("ROOST_UPDATE_DAO_SYNC_GOLDEN") == "1" {
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("collection golden drift: %v", err)
		}
	}
}

func TestTrackedChangesRollbackAndLaterWrite(t *testing.T) {
	d := collectionHero(128)
	if err := syncMutation(t, func() error { d.SetItems(1, 11); d.GetPos().SetX(12); return nil }); err != nil {
		t.Fatal(err)
	}
	captured := d.DirtyTracker().SyncChangeVersion()
	before := d.MarshalSyncChanges(^uint64(0))
	refused := errors.New("refused")
	if err := syncMutation(t, func() error { d.SetItems(2, 22); d.GetPos().SetY(99); d.AddFriends(123); return refused }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, d.MarshalSyncChanges(^uint64(0))) {
		t.Fatal("rollback left change records")
	}
	if err := syncMutation(t, func() error { d.SetItems(3, 33); return nil }); err != nil {
		t.Fatal(err)
	}
	d.DirtyTracker().AcknowledgeSyncChanges(captured)
	patch, _ := UnmarshalSyncHeroDao(d.MarshalSyncChanges(heroDaoFieldItems))
	if len(patch.Items.Value) != 1 || patch.Items.Value[3] != 33 {
		t.Fatal("ack consumed later write or retained acknowledged write")
	}
}

func TestTrackedInvalidPatchIsAtomic(t *testing.T) {
	d := collectionHero(8)
	before := d.MarshalSync(^uint64(0))
	for _, patch := range []*SyncHeroDao{
		{EntityID: 42, SyncFormat: 2},
		{EntityID: 42, SyncFormat: 3, Name: &SyncHeroDaoNameField{Value: "bad"}, Friends: &SyncHeroDaoFriendsField{Delta: true, Length: 10, Updates: map[uint64]int64{9: 9}}},
		{EntityID: 42, SyncFormat: 3, Friends: &SyncHeroDaoFriendsField{Delta: true, Length: 8, Updates: map[uint64]int64{99: 9}}},
		{EntityID: 42, SyncFormat: 3, Equips: &SyncHeroDaoEquipsField{Delta: true, Patches: map[int64]SyncPatchEquipInfo{99: {Level: &SyncPatchEquipInfoLevelField{Value: 99}}}}},
	} {
		raw, _ := MarshalSyncHeroDao(patch)
		if err := d.ApplySync(raw); err == nil {
			t.Fatal("invalid patch accepted")
		}
		if !bytes.Equal(before, d.MarshalSync(^uint64(0))) {
			t.Fatal("partial mutation")
		}
	}
}

func TestTrackedIndexedMutationsAndRollback(t *testing.T) {
	d := collectionHero(128)
	before := d.MarshalSync(^uint64(0))
	refused := errors.New("refused")
	err := syncMutation(t, func() error {
		d.SetFriendsAt(100, 99)
		d.SetFriendsAll([]int64{1, 2})
		d.SetFriendsAt(1, 888)
		d.SetFriendsAll(nil)
		item, _ := d.GetEquips(2)
		item.SetRunes([]*GemInfo{{level: 1}, {level: 2}})
		item.SetRunesAt(1, &GemInfo{level: 3})
		item.SetRunes(nil)
		item.SetGemsKey(3, &GemInfo{level: 7})
		item.SetGems(nil)
		item.SetGemsKey(4, &GemInfo{level: 8})
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, d.MarshalSync(^uint64(0))) {
		t.Fatal("mixed index/container rollback lost original state")
	}
}

func BenchmarkTrackedSyncCollections(b *testing.B) {
	for _, size := range []int{100, 1000} {
		for _, dense := range []bool{false, true} {
			name := fmt.Sprintf("n%d/dense%t", size, dense)
			d := collectionHero(size)
			if err := syncMutation(b, func() error {
				count := 1
				if dense {
					count = size
				}
				for i := range count {
					d.SetItems(int64(i), 999)
					d.SetFriendsAt(i, 7777)
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			mask := heroDaoFieldItems | heroDaoFieldFriends
			for _, tracked := range []bool{false, true} {
				b.Run(fmt.Sprintf("%s/tracked%t", name, tracked), func(b *testing.B) {
					var raw []byte
					b.ReportAllocs()
					for b.Loop() {
						if tracked {
							raw = d.MarshalSyncChanges(mask)
						} else {
							raw = d.MarshalSync(mask)
						}
					}
					b.ReportMetric(float64(len(raw)), "wire-B/op")
				})
			}
		}
	}
}

// 包含 setter、Nest 隔离事务/undo 登记、编码和变化确认；提交器是内存替身。
func BenchmarkTrackedSyncMutation(b *testing.B) {
	for _, tracked := range []bool{false, true} {
		b.Run(fmt.Sprintf("n1000/tracked%t", tracked), func(b *testing.B) {
			d := collectionHero(1000)
			mask := heroDaoFieldItems | heroDaoFieldFriends
			var raw []byte
			sequence := int64(2000)
			b.ReportAllocs()
			for b.Loop() {
				sequence++
				if err := syncMutation(b, func() error { d.SetItems(5, int32(sequence)); d.SetFriendsAt(50, sequence); return nil }); err != nil {
					b.Fatal(err)
				}
				if tracked {
					raw = d.MarshalSyncChanges(mask)
				} else {
					raw = d.MarshalSync(mask)
				}
				d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
			}
			b.ReportMetric(float64(len(raw)), "wire-B/op")
		})
	}
}
