//go:build daoruntime

// ARCH-06：生成的同步字段词汇表要可信，唯一的判据是"它报的 bit 与 setter
// 实际点亮的位一致"。文本比对做不到这件事——两边都从同一个模板来，一起写错
// 也一起绿。这里改一个字段、读真正的脏掩码、再和表对照。
package testdata

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

func TestSyncFieldTableAgreesWithWhatTheSettersMark(t *testing.T) {
	fields := HeroDaoSyncFields()
	if len(fields) == 0 {
		t.Fatal("the DAO reports no replicated fields")
	}
	// Every bit is distinct and non-zero: a vocabulary with a duplicate entry
	// is worse than none, because a consumer would confidently name the wrong
	// field.
	seen := make(map[uint64]string, len(fields))
	for _, field := range fields {
		if field.Bit == 0 {
			t.Errorf("field %s has no bit", field.Name)
		}
		if other, duplicate := seen[field.Bit]; duplicate {
			t.Errorf("fields %s and %s share bit %d", other, field.Name, field.Bit)
		}
		seen[field.Bit] = field.Name
		if field.WireName == "" {
			t.Errorf("field %s has no wire name", field.Name)
		}
	}

	// The promise: change one field, and the table names exactly that field.
	hero := newHero()
	hero.SetId(42)
	hero.DirtyTracker().TakeSyncDirty()
	if _, err := nest.RunIsolatedTransaction(context.Background(), &ownershipCommitter{}, "rename",
		func() (any, error) { hero.SetName("renamed"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	mask := hero.DirtyTracker().TakeSyncDirty()
	named := dataengine.SyncFieldsOf(fields, mask)
	if len(named) != 1 || named[0].Name != "Name" {
		t.Fatalf("after setting Name the mask %d names %v, want exactly Name", mask, named)
	}
	// And the lookup finds it by either spelling.
	byGoName, ok := dataengine.SyncFieldByName(fields, "Name")
	if !ok || byGoName.Bit != mask {
		t.Fatalf("lookup by Go name = %+v (ok=%v), want bit %d", byGoName, ok, mask)
	}
	byWireName, ok := dataengine.SyncFieldByName(fields, "name")
	if !ok || byWireName.Bit != mask {
		t.Fatalf("lookup by wire name = %+v (ok=%v), want bit %d", byWireName, ok, mask)
	}
	// The wire name is what MarshalSync actually writes, which is the only
	// reason a consumer can trust it to address the payload.
	if payload := hero.MarshalSync(mask); len(payload) == 0 {
		t.Fatal("the mask the table named produced no sync payload")
	}
}
