package skill

// RR-20261005-NC-115：skillsync 的可见性策略按 mutation 携带的实体判断能否下发。cast / process 的 upsert 带着
// 完整快照（Caster / Owner），对应的 remove 旧实现只带 CastID / ProcessID，过滤器拿不到实体、当作“无主”放行，
// 观察者因此收到不可见实体的 cast / process 何时结束。remove 必须带上与 upsert 相同的归属实体。

import "testing"

func TestRemoveMutationsCarryTheEntityTheirUpsertCarried(t *testing.T) {
	before := RuntimeStateSnapshot{
		Casts:     []CastStateSnapshot{{ID: 3, ProgramID: "p", Caster: 7, PrimaryTarget: 8}},
		Processes: []ProcessStateSnapshot{{ID: 4, CastID: 3, ProgramID: "p", Owner: 7, LifecycleEntity: 8}},
	}
	var castRemove, processRemove *StateMutation
	mutations := diffRuntimeState(before, RuntimeStateSnapshot{})
	for index := range mutations {
		switch mutations[index].Kind {
		case StateMutationCastRemove:
			castRemove = &mutations[index]
		case StateMutationProcessRemove:
			processRemove = &mutations[index]
		}
	}
	if castRemove == nil || processRemove == nil {
		t.Fatalf("mutations = %+v, want a cast and a process remove", mutations)
	}
	if castRemove.Caster != 7 {
		t.Errorf("cast remove = %+v, want Caster 7 (the upsert's caster)", *castRemove)
	}
	if processRemove.Owner != 7 {
		t.Errorf("process remove = %+v, want Owner 7 (the upsert's owner)", *processRemove)
	}
}
