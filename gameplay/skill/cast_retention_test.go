package skill

import (
	"encoding/json"
	"errors"
	"testing"
)

// CompletedCastLimit 有界保留终态 cast：提交前失败（施法窗口 commit 付费不足、Cancel / Release 回调失败）的 cast
// 同样进完成队列、按上限回收；checkpoint 恢复按“所有 finished / failed”重建完成队列，与运行中的保留集合一致。
// 历史：RR-20261005-NC-117。

func failBeforeCommit(t *testing.T, runtime *Runtime, program *Program, tick *Tick) CastID {
	t.Helper()
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	*tick += 3
	if err := runtime.Advance(*tick); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("commit advance = %v, want ErrInsufficientResource", err)
	}
	if snapshot, _ := runtime.InspectCast(id); snapshot.Status != CastFailed || runtime.casts[id].committed {
		t.Fatalf("cast %d: status=%s committed=%v, want an uncommitted failure", id, snapshot.Status, runtime.casts[id].committed)
	}
	*tick += 10
	if err := runtime.Advance(*tick); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUncommittedFailedCastsStayWithinTheCompletedCastLimit(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "cast_window_interrupt.json")
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 0}})
	options := RuntimeOptions{CompletedCastLimit: 2}
	runtime := NewRuntime(host, options)
	tick := Tick(0)
	for range 5 {
		failBeforeCommit(t, runtime, program, &tick)
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit {
		t.Errorf("retained casts = %d (completed queue %d) after 5 pre-commit failures; CompletedCastLimit is %d", stats.Casts, stats.CompletedCasts, options.CompletedCastLimit)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRuntime(host, options, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatalf("restore after pre-commit failures: %v", err)
	}
	again, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		CompletedCastOrder []CastID `json:"completed_cast_order"`
	}
	_ = json.Unmarshal(checkpoint.Payload, &before)
	_ = json.Unmarshal(again.Payload, &after)
	if len(before.CompletedCastOrder) != len(after.CompletedCastOrder) {
		t.Fatalf("completed queue live=%v restored=%v: restore rebuilt a different retention set", before.CompletedCastOrder, after.CompletedCastOrder)
	}
}

// 快照保存完成的先后顺序，恢复不按 ID 重排：completedCastOrder 按完成顺序排队，pruneCompletedCastsLocked 从队头淘汰；
// Checkpoint 按 ID 序列化 casts，所以完成顺序要单独带上，否则恢复前后同一批后续输入淘汰掉的是不同的 cast。
// 历史：U-0169。

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

// CompletedCastLimit 只限制可淘汰的终态 cast：仍有待执行任务或仍被衍生物引用的 cast 超出上限也不淘汰，
// 淘汰跳过它、改淘汰下一个可淘汰的；引用清空后它和其他 cast 一样可淘汰。
func TestReferencedCompletedCastsSurviveTheRetentionBound(t *testing.T) {
	runtime := NewRuntime(nil, RuntimeOptions{CompletedCastLimit: 1})
	referenced := &castInstance{id: 1, status: CastFinished, committed: true, abilityFinished: true, pendingTasks: 1}
	plain := &castInstance{id: 2, status: CastFinished, committed: true, abilityFinished: true}
	newest := &castInstance{id: 3, status: CastFinished, committed: true, abilityFinished: true}
	runtime.casts[1], runtime.casts[2], runtime.casts[3] = referenced, plain, newest
	runtime.activeCastCount = 3
	runtime.trackCompletedCastLocked(referenced)
	runtime.trackCompletedCastLocked(plain)
	runtime.trackCompletedCastLocked(newest)
	if runtime.casts[1] == nil {
		t.Fatal("a completed cast with a pending task was evicted by the retention bound")
	}
	if runtime.casts[2] != nil {
		t.Fatal("the evictable cast was kept while the referenced one should have been skipped over")
	}
	// Once its task drains it becomes evictable like any other.
	referenced.pendingTasks = 0
	extra := &castInstance{id: 4, status: CastFinished, committed: true, abilityFinished: true}
	runtime.casts[4] = extra
	runtime.activeCastCount = 1
	runtime.trackCompletedCastLocked(extra)
	if runtime.casts[1] != nil {
		t.Fatal("a cast whose references drained was still pinned")
	}
}
