package nest

import (
	"context"
	"math"
	"sync/atomic"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// pipelinedStubCommitter 只提供水位；本测试不跑事务，内容的 CommitLSN 手工设定。
type pipelinedStubCommitter struct {
	noOpCommitter
	durable atomic.Uint64
}

func (c *pipelinedStubCommitter) Enqueue(context.Context, corenest.CommitRecord) (corenest.CommitTicket, error) {
	return nil, corenest.ErrCommitterRequired
}
func (c *pipelinedStubCommitter) DurableLSN() uint64 { return c.durable.Load() }

// RR-20260926-35：kit 装配的 EntitySync 在 committer 为 pipelined 时自动以其 DurableLSN 作外发水位。
func TestEntitySyncModWiresPipelinedDurableWatermark(t *testing.T) {
	pipelined := &pipelinedStubCommitter{}
	for _, tc := range []struct {
		name      string
		committer corenest.TransactionCommitter
		explicit  func() uint64
		gated     bool
	}{
		{name: "pipelined_committer", committer: pipelined, gated: true},
		{name: "explicit_watermark_wins", committer: pipelined, explicit: func() uint64 { return math.MaxUint64 }},
		{name: "strict_committer_has_no_gate", committer: noOpCommitter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipelined.durable.Store(0)
			sent := make(chan []byte, 8)
			setup := EntitySyncSetup{Config: entitysync.ManagerConfig{DurableWatermark: tc.explicit, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
				sent <- p
				return nil
			})}}
			mod := NewModWithEntitySync(emptyGetter{}, setup)
			cfg := viper.New()
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			registry := app.NewRegistry(cfg)
			if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: tc.committer}); err != nil {
				t.Fatal(err)
			}
			if err := mod.Provide(registry); err != nil {
				t.Fatal(err)
			}
			manager := mod.EntitySync()
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			payload := entity.CopyFrozenSyncPayload(1, []byte{9})
			pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) { return payload, nil }
			state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 4242, Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
			state.SetLastCommitLSN(5) // 一次尚未持久的 pipelined 提交产生的内容
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			if err := manager.OpenSession(1); err != nil {
				t.Fatal(err)
			}
			if err := manager.Subscribe(1, 4242, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(sent); tc.gated && got != 0 {
				t.Fatalf("content above the pipelined durable watermark was sent (%d frames); kit did not wire DurableLSN", got)
			} else if !tc.gated && got != 1 {
				t.Fatalf("frames=%d, want 1 (no pipelined watermark applies)", got)
			}
			if !tc.gated {
				return
			}
			pipelined.durable.Store(5)
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(sent); got != 1 {
				t.Fatalf("frames after the watermark reached the commit = %d, want 1", got)
			}
		})
	}
}
