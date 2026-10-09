package skill

import (
	"testing"
)

// U-0169 · C8 · RR-20260910-04:快照必须保存完成的先后顺序,而不是让恢复按 ID 重排。
//
// completedCastOrder 是"按完成顺序"的队列,pruneCompletedCastsLocked 从队头开始淘汰。而
// Checkpoint 把 casts 按 ID 排序序列化,Restore 又按这个顺序重建队列:创建早但完成晚的技能
// 会被排到前面,恢复之后同一批后续输入淘汰掉的是另一个 cast。两条路径对"谁最旧"的判据不同,
// 历史保留与诊断查询因此在恢复前后分叉。

// 序列化侧:payload 必须把运行时的完成顺序单独带上,因为 Casts 是按 ID 序列化的。
func TestCheckpointCarriesTheCompletionOrderSeparately(t *testing.T) {
	program, environment := compileCastWindowSkill(t, true)
	source := NewRuntime(runtimeTestHost(environment), RuntimeOptions{
		MatchSeed: fixedTestSeed(91), CompletedCastLimit: 2,
	})
	early, err := source.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	// A different caster: the window skill allows one active cast per caster.
	late, err := source.Activate(program, CastInput{Caster: 2, Target: 3})
	if err != nil {
		t.Fatal(err)
	}
	if early >= late {
		t.Fatalf("expected ascending cast ids, got early=%d late=%d", early, late)
	}
	// Advance once so the host-state baseline is established; the payload
	// builder refuses otherwise.
	if err := source.Advance(2); err != nil {
		t.Fatal(err)
	}
	// The one created LATER completed FIRST, so completion order and id order
	// disagree — which is the whole point.
	source.completedCastOrder = []CastID{late, early}

	payload, err := source.checkpointPayloadLocked()
	if err != nil {
		t.Fatal(err)
	}
	if got := payload.CompletedCastOrder; len(got) != 2 || got[0] != late || got[1] != early {
		t.Fatalf("the payload carries completion order %v, want %v", got, []CastID{late, early})
	}
	if len(payload.Casts) != 2 || payload.Casts[0].ID != early || payload.Casts[1].ID != late {
		t.Fatalf("expected casts serialized in id order, got %v", payload.Casts)
	}
	// The payload must own its copy: later runtime activity cannot rewrite a
	// checkpoint that was already taken.
	source.completedCastOrder[0] = early
	if payload.CompletedCastOrder[0] != late {
		t.Fatal("the payload aliases the runtime's slice")
	}
}

// 恢复侧:记录的顺序优先,并且必须是确定的。
func TestRestoreCompletedCastOrderPrefersTheRecordedOrder(t *testing.T) {
	idOrdered := []checkpointCast{{ID: 1}, {ID: 2}, {ID: 3}}
	for _, tc := range []struct {
		name      string
		recorded  []CastID
		completed map[CastID]bool
		want      []CastID
	}{
		{
			"recorded order wins over id order",
			[]CastID{3, 1, 2}, map[CastID]bool{1: true, 2: true, 3: true},
			[]CastID{3, 1, 2},
		},
		{
			"a checkpoint written before the field falls back to id order",
			nil, map[CastID]bool{1: true, 2: true, 3: true},
			[]CastID{1, 2, 3},
		},
		{
			"ids that did not come back terminal are dropped, not resurrected",
			[]CastID{3, 9, 1}, map[CastID]bool{1: true, 3: true},
			[]CastID{3, 1},
		},
		{
			"a terminal cast the order forgot is appended in id order",
			[]CastID{3}, map[CastID]bool{1: true, 2: true, 3: true},
			[]CastID{3, 1, 2},
		},
		{
			"a duplicated id is placed once",
			[]CastID{2, 2, 1}, map[CastID]bool{1: true, 2: true},
			[]CastID{2, 1},
		},
		{
			"nothing terminal is an empty queue, not a nil surprise",
			[]CastID{1, 2}, map[CastID]bool{},
			[]CastID{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreCompletedCastOrder(tc.recorded, tc.completed, idOrdered)
			if len(got) != len(tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("order = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// 后果:淘汰从队头走,所以恢复后的队列顺序决定了同一批后续输入淘汰掉谁。
func TestRestoredCompletionOrderDecidesWhichCastRetentionEvicts(t *testing.T) {
	const early, late CastID = 1, 2
	for _, tc := range []struct {
		name     string
		recorded []CastID
		evicted  CastID
		kept     CastID
	}{
		{"completion order evicts the oldest completion", []CastID{late, early}, late, early},
		{"id order would have evicted the other one", nil, early, late},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := NewRuntime(nil, RuntimeOptions{CompletedCastLimit: 1})
			runtime.casts[early] = &castInstance{id: early, status: CastFinished, committed: true, abilityFinished: true}
			runtime.casts[late] = &castInstance{id: late, status: CastFinished, committed: true, abilityFinished: true}
			runtime.completedCastOrder = restoreCompletedCastOrder(
				tc.recorded,
				map[CastID]bool{early: true, late: true},
				[]checkpointCast{{ID: early}, {ID: late}},
			)
			runtime.pruneCompletedCastsLocked()
			if runtime.casts[tc.evicted] != nil {
				t.Errorf("cast %d should have been evicted from the head", tc.evicted)
			}
			if runtime.casts[tc.kept] == nil {
				t.Errorf("cast %d should have been kept", tc.kept)
			}
		})
	}
}
