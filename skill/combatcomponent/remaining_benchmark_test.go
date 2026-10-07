package combatcomponent

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/skill/combat"
)

func BenchmarkCombatRepeatedAttributeWrites(b *testing.B) {
	component := NewCombatComponent(NewCombatDao(1, "benchmark"))
	for id := combat.AttributeID(1); id <= 1000; id++ {
		component.dao.attributes.SetBase(id, 100)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "combat_attribute_bench", func() (any, error) {
			for value := int64(0); value < 100; value++ {
				component.SetAttributeBase(1, value)
			}
			return nil, nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
