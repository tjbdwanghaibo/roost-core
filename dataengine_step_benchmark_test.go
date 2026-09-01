package saga

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func BenchmarkDataEngineStepReservation(b *testing.B) {
	b.Run("new_command", func(b *testing.B) {
		client := newDataEngineInboxMongo()
		inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "bench", LeaseDuration: time.Minute})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			command := dataEngineCommand(fmt.Sprintf("command-%d", index), fmt.Sprintf("operation-%d", index), "payload")
			if _, err := inbox.Reserve(context.Background(), command); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("duplicate_active_claim", func(b *testing.B) {
		client := newDataEngineInboxMongo()
		inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "bench", LeaseDuration: time.Minute})
		if err != nil {
			b.Fatal(err)
		}
		command := dataEngineCommand("duplicate-command", "duplicate-operation", "payload")
		if _, err := inbox.Reserve(context.Background(), command); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if _, err := inbox.Reserve(context.Background(), command); err != nil {
				b.Fatal(err)
			}
		}
	})
}
