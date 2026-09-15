package statesync

import "testing"

// U-0203 · C8 · RR-20260914-13:限频组件的刷新不能只看绝对 tick 取模。`refreshDue` 是
// `tick%interval == 0`,而会话并不在每个 tick 都发送:SnapshotRateHz=20、MaxRateHz=10(interval=2)
// 的会话只在奇数 tick 发送时,永远踩不到偶数采样点,普通组件从首次全量之后一直保留旧值——
// ACK 正常也救不回来,因为 ACK 的投影本身就是旧值。承诺:自上次发送以来跨过了一个采样点就刷新
// (每个采样区间至多刷新一次,限频语义不变),与发送相位无关;逐 tick 发送的行为不变。
func TestLODProjectorPromiseRefreshDoesNotDependOnSendPhase(t *testing.T) {
	for _, mode := range []string{"all_ticks", "odd_ticks", "odd_full_refresh"} {
		t.Run(mode, func(t *testing.T) {
			check := func(e error) {
				t.Helper()
				if e != nil {
					t.Fatal(e)
				}
			}
			projector, e := NewLODProjector(LODProjectorConfig{
				Registry: testLODRegistry(t), SnapshotRateHz: 20,
				Selector: LODSelectorFunc(func(ProjectionContext, ObjectState) (LODDecision, error) {
					return LODDecision{Level: LODFull, MaxRateHz: 10}, nil
				}),
			})
			check(e)
			r := NewReplicator(ReplicatorConfig{Projector: projector})
			defer r.Close()
			check(r.RegisterSession(SessionInfo{ID: 10}))
			var client *Snapshot
			for tick := uint32(1); tick <= 9; tick++ {
				check(r.Publish(mustSnapshot(t, tick, []ObjectState{lodObject(byte(tick), byte(tick), byte(tick))})))
				if mode != "all_ticks" && tick%2 == 0 {
					continue
				}
				if mode == "odd_full_refresh" {
					check(r.ForceFull(10))
				}
				p, e := r.PrepareLatest(10)
				check(e)
				next, e := ApplyDelta(client, p.Frame, DefaultLimits())
				check(e)
				check(p.Commit())
				check(r.Acknowledge(10, tick))
				client = &next
			}
			got := client.Objects[0].Components[1].Data[0]
			if got < 8 {
				t.Fatalf("client at tick=%d normal=%d want >=8 after multiple rate intervals", client.Tick, got)
			}
			if mode == "all_ticks" && got != 8 {
				// 逐 tick 发送:tick 9 与 tick 8 同一采样区间,合法保留 tick 8 的采样值——限频语义未被放宽。
				t.Fatalf("per-tick sends: tick 9 must still hold the sample from tick 8, got %d", got)
			}
		})
	}
}
