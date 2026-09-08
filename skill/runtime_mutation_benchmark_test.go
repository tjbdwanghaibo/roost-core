package skill

import "testing"

func BenchmarkStateMutationCommit1000Cooldowns(b *testing.B) {
	host := NewMemoryHost(AuthorityIdentity{})
	runtime := NewRuntime(host, RuntimeOptions{})
	for index := 1; index <= 1000; index++ {
		key := cooldownKey{Caster: EntityID(index), Skill: "skill"}
		runtime.cooldowns[key] = 10
		runtime.touchCooldownLocked(key)
	}
	runtime.beginStateMutationLocked()
	runtime.commitStateMutationsLocked()
	stateMutationVerifyIncremental = false
	b.Cleanup(func() { stateMutationVerifyIncremental = true })
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		runtime.beginStateMutationLocked()
		runtime.commitStateMutationsLocked()
	}
}

func BenchmarkStateMutationCommit1000CooldownsTickAdvance(b *testing.B) {
	host := NewMemoryHost(AuthorityIdentity{})
	runtime := NewRuntime(host, RuntimeOptions{})
	for index := 1; index <= 1000; index++ {
		key := cooldownKey{Caster: EntityID(index), Skill: "skill"}
		runtime.cooldowns[key] = Tick(1 << 40)
		runtime.touchCooldownLocked(key)
	}
	runtime.beginStateMutationLocked()
	runtime.commitStateMutationsLocked()
	stateMutationVerifyIncremental = false
	b.Cleanup(func() { stateMutationVerifyIncremental = true })
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		runtime.currentTick++
		runtime.beginStateMutationLocked()
		runtime.commitStateMutationsLocked()
	}
}
