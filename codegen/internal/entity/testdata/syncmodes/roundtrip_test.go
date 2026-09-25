package syncmodes

import (
	"context"
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"testing"
	"time"
)

type getter struct{ entity *Avatar }

func (g getter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.entity, nil
}
func (g getter) GetMany(context.Context, []int64, []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return []entity.IThreadSafeEntity{g.entity}, nil
}
func TestGeneratedDAOChangesReachBothSyncModes(t *testing.T) {
	RegisterEntity()
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			id, _ := entity.BuildEntityID(1, EntityKindAvatar)
			built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindAvatar, Id: id})
			if err != nil {
				t.Fatal(err)
			}
			avatar := built.(*Avatar)
			frames := make(chan []byte, 8)
			m, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: mode, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error { frames <- p; return nil })})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			if err = m.Register(avatar.Sync()); err != nil {
				t.Fatal(err)
			}
			if err = m.OpenSession(1); err != nil {
				t.Fatal(err)
			}
			if err = m.Subscribe(1, id, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			if err = m.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-frames
			name := nest.NewHandlerName("generated_set_" + mode.String())
			nest.MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
				es[0].(*Avatar).state.SetValue(42)
				return nil, nil
			})
			engine := nest.NewEngine(nest.NestOptionWithGetter(getter{avatar}), nest.NestOptionWithEntitySync(m))
			if err = engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())
			if _, err = engine.Request(context.Background(), name, id, nil); err != nil {
				t.Fatal(err)
			}
			if err = m.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case raw := <-frames:
				f, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				update, err := entitysync.DecodeSubjectUpdate(f.Objects[0].Components[0].Data, 0)
				if err != nil {
					t.Fatal(err)
				}
				if got := binary.LittleEndian.Uint64(update.Payload.BytesCopy()); got != 42 {
					t.Fatalf("value %d", got)
				}
			default:
				t.Fatal("generated setter did not publish")
			}
			if avatar.state.DirtyTracker().TakeSyncDirty() == 0 {
				t.Fatal("client consumed server sync mask")
			}
		})
	}
}
