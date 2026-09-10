package entity

import (
	"testing"
)

// M-02 · category 要能超过 ID 那两位能表达的三个值。
//
// 锁序就是 category 的值,而目标形态需要五档:remote、world、player-scoped、
// player、other。ID 的低两位只能表达 1 到 3,所以 category 必须先离开 ID:
// 注册表是权威,ID 里那两位降为无人读取的历史填充,老 ID 位级不变、零数据迁移。
func TestEntityCategoriesBeyondTheIDMaskAreAllowed(t *testing.T) {
	// No reset: the package's init registers shared test kinds.
	const (
		fourth  EntityKind     = 161
		fifth   EntityKind     = 162
		catFour EntityCategory = EntityCategory(EntityCategoryMask) + 1
		catFive EntityCategory = EntityCategory(EntityCategoryMask) + 2
	)

	if err := RegisterEntityKindCategory(fourth, catFour); err != nil {
		t.Fatalf("registering category %d must be allowed once category left the ID: %v", catFour, err)
	}
	if err := RegisterEntityKindCategory(fifth, catFive); err != nil {
		t.Fatalf("registering category %d must be allowed: %v", catFive, err)
	}

	// The registry, not the ID's low bits, answers what a kind's category is.
	if got, ok := EntityCategoryOfKind(fourth); !ok || got != catFour {
		t.Fatalf("EntityCategoryOfKind = (%d, %v), want (%d, true)", got, ok, catFour)
	}

	// IDs still round-trip: kind and unique id survive, and the id resolves to
	// the registered category even though it cannot fit in the low two bits.
	id, err := BuildEntityID(1234, fourth)
	if err != nil {
		t.Fatalf("BuildEntityID for a kind above the mask: %v", err)
	}
	meta := ResolveEntityID(id)
	if meta.Kind != fourth {
		t.Fatalf("resolved kind = %d, want %d", meta.Kind, fourth)
	}
	if meta.UniqueID != 1234 {
		t.Fatalf("resolved unique id = %d, want 1234", meta.UniqueID)
	}
	if meta.Category != catFour {
		t.Fatalf("resolved category = %d, want the registered %d", meta.Category, catFour)
	}
	if _, err := NormalizeFullID(id, fourth); err != nil {
		t.Fatalf("NormalizeFullID must accept a kind whose category exceeds the mask: %v", err)
	}
}
