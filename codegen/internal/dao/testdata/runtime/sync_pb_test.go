//go:build daoruntime

package testdata

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestSyncPBZeroEmptyAndNullableFields(t *testing.T) {
	source := newHero()
	source.SetId(9007199254740993)
	source.name = "hello"
	source.level = 7
	source.exp = 1 << 60
	source.loginAt = 123
	source.friends = []int64{8}
	source.setItemsRawMap(map[int64]int32{3: 4})
	source.setEquipsRawMap(map[int64]*EquipInfo{4: nil})
	source.squad = []*EquipInfo{nil}
	source.mount = nil
	full := source.MarshalSync(^uint64(0))
	patch, err := UnmarshalSyncHeroDao(full)
	if err != nil {
		t.Fatal(err)
	}
	if patch.EntityID != 9007199254740993 || patch.Exp.Value != 1<<60 {
		t.Fatal("64 bit identity/value lost")
	}
	values, err := patch.Values()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["login_at"]; ok {
		t.Fatal("persist-only field leaked")
	}
	mirror := NewHeroDao()
	if err := mirror.ApplySync(full); err != nil {
		t.Fatal(err)
	}
	if got, ok := mirror.GetEquips(4); !ok || got != nil {
		t.Fatal("nil map value lost")
	}
	squad, ok := mirror.GetSquad(0)
	if mirror.SquadLen() != 1 || !ok || squad != nil {
		t.Fatal("nil slice element lost")
	}
	source.name = ""
	source.level = 0
	source.friends = nil
	source.setItemsRawMap(nil)
	delta := source.MarshalSync(heroDaoFieldName | heroDaoFieldLevel | heroDaoFieldFriends | heroDaoFieldItems | heroDaoFieldMount)
	if err := mirror.ApplySync(delta); err != nil {
		t.Fatal(err)
	}
	if mirror.GetName() != "" || mirror.GetLevel() != 0 || mirror.ItemsLen() != 0 || mirror.FriendsLen() != 0 || mirror.GetExp() != 1<<60 {
		t.Fatal("zero/empty patch lost or untouched field reset")
	}
	before, _ := mirror.CaptureRollbackState()
	if err := mirror.ApplySync(delta[:len(delta)-1]); err == nil {
		t.Fatal("truncated patch accepted")
	}
	after, _ := mirror.CaptureRollbackState()
	if !bytes.Equal(before, after) {
		t.Fatal("bad PB partially applied")
	}
	if payload := source.MarshalSync(heroDaoFieldLoginAt); len(payload) != 0 {
		t.Fatal("persist-only mask emitted payload")
	}

	numeric := NewVarietyDao()
	numeric.enabled = true
	numeric.counter = ^uint64(0)
	numericMirror := NewVarietyDao()
	if err := numericMirror.ApplySync(numeric.MarshalSync(^uint64(0))); err != nil {
		t.Fatal(err)
	}
	if !numericMirror.GetEnabled() || numericMirror.GetCounter() != ^uint64(0) {
		t.Fatal("PB bool/uint64 lost")
	}
	numeric.enabled = false
	numeric.counter = 0
	if err := numericMirror.ApplySync(numeric.MarshalSync(varietyDaoFieldEnabled | varietyDaoFieldCounter)); err != nil {
		t.Fatal(err)
	}
	if numericMirror.GetEnabled() || numericMirror.GetCounter() != 0 {
		t.Fatal("PB false/zero patch lost")
	}

	// 与 C# 共用真正由生成 DAO 写出的字节，不手写另一份“等效”编码器。
	if path := os.Getenv("ROOST_DAO_SYNC_GOLDEN"); path != "" {
		rows := []struct {
			Name string `json:"name"`
			Hex  string `json:"hex"`
		}{{"full", hex.EncodeToString(full)}, {"zero_empty", hex.EncodeToString(delta)}}
		raw, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		if os.Getenv("ROOST_UPDATE_DAO_SYNC_GOLDEN") == "1" {
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, raw) {
			t.Fatal("DAO PB cross-language golden drift")
		}
	}
}
