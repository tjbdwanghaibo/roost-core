package entitysync

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"testing"
	"testing/synctest"
	"time"
)

func batchManager(t *testing.T, window, interval time.Duration) *Manager {
	t.Helper()
	m, err := NewManager(ManagerConfig{Mode: ModeOnChange, OnChangeBatchWindow: window, Interval: interval, Transport: TransportFunc(func(context.Context, SessionID, []byte) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	m.BindSyncProducer()
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	return m
}

// 虚拟时间验证固定截止，而非依赖机器调度误差的 Sleep 容差。
func TestOnChangeBatchWindowDoesNotSlide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := batchManager(t, 10*time.Millisecond, time.Hour)
		defer m.Close(context.Background())
		for range 10 {
			m.WakeSync()
			synctest.Wait()
			if m.flushCalls.Load() != 0 {
				t.Fatal("flushed before fixed window")
			}
			time.Sleep(time.Millisecond)
		}
		synctest.Wait()
		if got := m.flushCalls.Load(); got != 1 {
			t.Fatalf("continuous wakes delayed or duplicated flush: %d", got)
		}
		m.WakeSync()
		synctest.Wait()
		time.Sleep(9 * time.Millisecond)
		synctest.Wait()
		if m.flushCalls.Load() != 1 {
			t.Fatal("next window ended early")
		}
		start := time.Now()
		if err := m.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 0 || m.flushCalls.Load() != 2 {
			t.Fatal("Stop waited for batch window")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if m.flushCalls.Load() != 2 {
			t.Fatal("stopped timer still flushed")
		}
	})
}

func TestOnChangeBatchWindowTickAndExplicitFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := batchManager(t, 10*time.Millisecond, 7*time.Millisecond)
		defer m.Close(context.Background())
		m.WakeSync()
		synctest.Wait()
		time.Sleep(7 * time.Millisecond)
		synctest.Wait()
		if m.flushCalls.Load() != 1 {
			t.Fatal("tick did not preempt batch")
		}
		time.Sleep(3 * time.Millisecond)
		synctest.Wait()
		if m.flushCalls.Load() != 1 {
			t.Fatal("preempted timer flushed again")
		}
		m.WakeSync()
		synctest.Wait()
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m.flushCalls.Load() != 2 {
			t.Fatal("explicit Flush was delayed")
		}
	})
}

func TestOnChangeZeroWindowAndConfiguration(t *testing.T) {
	for _, c := range []ManagerConfig{{Mode: ModeOnChange, OnChangeBatchWindow: -1}, {Mode: ModePeriodic, OnChangeBatchWindow: time.Millisecond}} {
		c.Transport = TransportFunc(func(context.Context, SessionID, []byte) error { return nil })
		if _, err := NewManager(c); err == nil {
			t.Fatal("invalid batch configuration accepted")
		}
	}
	synctest.Test(t, func(t *testing.T) {
		m := batchManager(t, 0, time.Hour)
		defer m.Close(context.Background())
		for i := uint64(1); i <= 10; i++ {
			m.WakeSync()
			synctest.Wait()
			if m.flushCalls.Load() != i {
				t.Fatal("zero window changed immediate behavior")
			}
		}
	})
}

func TestOnChangeBatchWindowCancelAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, err := NewManager(ManagerConfig{Mode: ModeOnChange, OnChangeBatchWindow: 10 * time.Millisecond, Interval: time.Hour, Transport: newRecordingTransport()})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close(context.Background())
		m.BindSyncProducer()
		ctx, cancel := context.WithCancel(context.Background())
		if err = m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		m.WakeSync()
		synctest.Wait()
		cancel()
		synctest.Wait()
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		if m.flushCalls.Load() != 0 {
			t.Fatal("canceled loop flushed its old timer")
		}
		if err = m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.WakeSync()
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if m.flushCalls.Load() != 1 {
			t.Fatal("restarted loop did not use a fresh window")
		}
	})
}

func TestOnChangeBatchWindowPreservesDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newRecordingTransport()
		m, err := NewManager(ManagerConfig{Mode: ModeOnChange, OnChangeBatchWindow: 10 * time.Millisecond, Interval: time.Hour, Transport: tr})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close(context.Background())
		packs := 0
		s := testSubject(t, 1, &packs)
		if err = m.Register(s); err != nil {
			t.Fatal(err)
		}
		open(t, m, 1)
		mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
		m.BindSyncProducer()
		if err = m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.WakeSync()
		synctest.Wait()
		if len(tr.take(1)) != 0 {
			t.Fatal("delivery bypassed window")
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		oneFrame(t, tr, 1)
		for range 3 {
			s.MarkDirty(1)
			if err = m.CaptureSync(s); err != nil {
				t.Fatal(err)
			}
			m.WakeSync()
			synctest.Wait()
			time.Sleep(time.Millisecond)
		}
		if len(tr.take(1)) != 0 {
			t.Fatal("update bypassed window")
		}
		time.Sleep(7 * time.Millisecond)
		synctest.Wait()
		oneFrame(t, tr, 1)
		if m.Counters().FrozenBytes != 0 {
			t.Fatal("delivered batch retained frozen payload")
		}
	})
}

func TestOnChangeBatchWindowBurstComparison(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		frames int
	}{{0, 20}, {5 * time.Millisecond, 4}, {10 * time.Millisecond, 2}} {
		t.Run(tc.window.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tr := newRecordingTransport()
				m, err := NewManager(ManagerConfig{Mode: ModeOnChange, OnChangeBatchWindow: tc.window, Interval: time.Hour, Transport: tr})
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close(context.Background())
				value := byte(0)
				pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
					return entity.CopyFrozenSyncPayload(1, []byte{value}), nil
				}
				s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 1, Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
				if err = m.Register(s); err != nil {
					t.Fatal(err)
				}
				open(t, m, 1)
				mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
				mustFlush(t, m)
				version := oneFrame(t, tr, 1).updates[1].Version
				m.BindSyncProducer()
				if err = m.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				for range 20 {
					value++
					s.MarkDirty(1)
					if err = m.CaptureSync(s); err != nil {
						t.Fatal(err)
					}
					m.WakeSync()
					synctest.Wait()
					time.Sleep(time.Millisecond)
				}
				synctest.Wait()
				frames := tr.take(1)
				if len(frames) != tc.frames {
					t.Fatalf("frames %d want %d", len(frames), tc.frames)
				}
				for _, encoded := range frames {
					update := decodeFrame(t, encoded).updates[1]
					if update.Version <= version || !update.Full && update.BaseVersion != version {
						t.Fatal("invalid delivered version chain")
					}
					version = update.Version
				}
				if got := decodeFrame(t, frames[len(frames)-1]).updates[1].Payload.BytesCopy(); len(got) != 1 || got[0] != 20 {
					t.Fatalf("latest state lost: %v", got)
				}
				if m.Counters().FrozenBytes != 0 {
					t.Fatal("pending frozen updates")
				}
				t.Logf("20 captures spaced 1ms, window=%v, delivered frames=%d", tc.window, len(frames))
			})
		})
	}
}
