package skill

import (
	"errors"
	"testing"
)

type neutralProbeHost struct {
	*MemoryHost
	multiplier int64
}

func (host *neutralProbeHost) Apply(command EffectCommand) (EffectResult, error) {
	if modifier, ok := command.Payload.(AttributeModifierCommand); ok && modifier.Operation == "mul_bp" {
		host.multiplier = modifier.Value
	}
	return host.MemoryHost.Apply(command)
}

func TestCapabilityProbeIsNeutralAndStopsItsSpawn(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &neutralProbeHost{MemoryHost: hostCapabilityProbeHost(environment), multiplier: -1}
	if err := CheckHostCapabilities(host, environment.Gameplay, HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatal(err)
	}
	if host.multiplier != 10000 {
		t.Errorf("mul_bp probe=%d want neutral 10000", host.multiplier)
	}
	for id, spawn := range host.spawns {
		if spawn.active {
			t.Errorf("capability probe left active spawn %d", id)
		}
	}
}

type nameOnlyPaymentHost struct{ *MemoryHost }

func (host *nameOnlyPaymentHost) PayCosts(payment CostPayment) (CommitReceipt, error) {
	for _, entry := range payment.Entries {
		if entry.Resource == "" {
			return CommitReceipt{}, errors.New("only resource names supported")
		}
	}
	return host.MemoryHost.PayCosts(payment)
}
func TestCapabilityProbeUsesRuntimeHandleOnlyPayment(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &nameOnlyPaymentHost{MemoryHost: hostCapabilityProbeHost(environment)}
	if err := CheckHostCapabilities(host, environment.Gameplay, HostCapabilityProbe{Entity: 1}); err == nil {
		t.Fatal("name-only Host passed probe although Runtime pays with handles only")
	}
}

func TestDefaultMemoryHostSupportsItsDeclaredResourceHandles(t *testing.T) {
	program := summonCostingProgram(t)
	host := NewMemoryHost(program.AuthorityIdentity())
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Resources: map[string]int64{"mana": 100}})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatalf("default advertised catalog cannot execute cost/summon: %v", err)
	}
	if mana := host.ResourceForTest(1, "mana"); mana != 90 {
		t.Fatalf("mana=%d", mana)
	}
}
