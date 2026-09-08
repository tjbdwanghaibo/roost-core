package skill

import (
	"errors"
	"strconv"
	"testing"
)

func TestCastPolicyTogglePulsesAndSecondActivateReleases(t *testing.T) {
	policy := `{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":8,"sustain_costs":[{"resource":"mana","amount":1}]}`
	program, environment := compilePolicySkill(t, "toggle", policy, 10)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.CooldownUntil(program, 1) != 0 {
		t.Fatal("toggle cooldown started before release")
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	if host.ResourceForTest(1, "mana") != 99 || host.HealthForTest(2) != 99 {
		t.Fatalf("pulse mismatch: mana=%d health=%d", host.ResourceForTest(1, "mana"), host.HealthForTest(2))
	}
	releasedID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil || releasedID != castID {
		t.Fatalf("toggle-off = %d %v", releasedID, err)
	}
	assertCastWindowState(t, runtime, castID, CastFinished, CastWindowComplete)
	if host.HealthForTest(2) != 97 || runtime.CooldownUntil(program, 1) != 12 {
		t.Fatalf("release mismatch: health=%d cooldown=%d", host.HealthForTest(2), runtime.CooldownUntil(program, 1))
	}
}

func TestHoldReleaseStopsPulsesAndStartsCooldown(t *testing.T) {
	policy := `{"mode":"hold","pulse_interval_ticks":2,"max_duration_ticks":8,"sustain_costs":[]}`
	program, environment := compilePolicySkill(t, "hold", policy, 6)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(2); err != nil || host.HealthForTest(2) != 99 {
		t.Fatalf("pulse = %v health=%d", err, host.HealthForTest(2))
	}
	if err := runtime.Release(castID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(8); err != nil || host.HealthForTest(2) != 97 {
		t.Fatalf("released hold continued pulsing: %v health=%d", err, host.HealthForTest(2))
	}
	if runtime.CooldownUntil(program, 1) != 8 {
		t.Fatalf("cooldown = %d", runtime.CooldownUntil(program, 1))
	}
}

func TestChargeBelowMinimumCancelsAndSuccessfulReleasePaysOnce(t *testing.T) {
	policy := `{"mode":"charge","max_charge_ticks":10,"min_charge_bp":5000,"auto_release":true}`
	program, environment := compilePolicySkill(t, "charge", policy, 10)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(4); err != nil || runtime.Release(castID) != nil {
		t.Fatal("early release failed")
	}
	if host.ResourceForTest(1, "mana") != 100 || host.HealthForTest(2) != 100 || runtime.CooldownUntil(program, 1) != 0 {
		t.Fatal("early charge release paid or executed")
	}

	castID, err = runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(9); err != nil || runtime.Release(castID) != nil {
		t.Fatal("successful release failed")
	}
	if host.ResourceForTest(1, "mana") != 95 || host.HealthForTest(2) != 98 || runtime.CooldownUntil(program, 1) != 19 {
		t.Fatalf("successful charge mismatch: mana=%d health=%d cooldown=%d", host.ResourceForTest(1, "mana"), host.HealthForTest(2), runtime.CooldownUntil(program, 1))
	}
}

func TestAmmoPaymentStockAndSingleRechargeTimeline(t *testing.T) {
	policy := `{"mode":"ammo","max_stock":2,"recharge_ticks":3,"initial_stock":2}`
	program, environment := compilePolicySkill(t, "ammo", policy, 0)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	for wantStock := int64(1); wantStock >= 0; wantStock-- {
		if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
			t.Fatal(err)
		}
		stock, _, _ := runtime.SkillStock(program, 1)
		if stock != wantStock {
			t.Fatalf("stock=%d want=%d", stock, wantStock)
		}
	}
	mana := host.ResourceForTest(1, "mana")
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("empty ammo = %v", err)
	}
	if host.ResourceForTest(1, "mana") != mana {
		t.Fatal("failed ammo activation paid cost")
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	stock, _, _ := runtime.SkillStock(program, 1)
	if stock != 1 {
		t.Fatalf("first recharge stock=%d", stock)
	}
	if err := runtime.Advance(6); err != nil {
		t.Fatal(err)
	}
	stock, _, _ = runtime.SkillStock(program, 1)
	if stock != 2 {
		t.Fatalf("second recharge stock=%d", stock)
	}
}

func TestToggleSustainFailureReleasesAndCancelStartsCooldown(t *testing.T) {
	policy := `{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":8,"sustain_costs":[{"resource":"mana","amount":1}]}`
	program, environment := compilePolicySkill(t, "toggle-depleted", policy, 6)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 0}})
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil || runtime.Advance(2) != nil {
		t.Fatal("depleted toggle failed")
	}
	snapshot, _ := runtime.InspectCast(castID)
	if snapshot.ReleaseReason != "resource_depleted" || snapshot.Status != CastFinished || runtime.CooldownUntil(program, 1) != 8 {
		t.Fatalf("depleted snapshot=%#v cooldown=%d", snapshot, runtime.CooldownUntil(program, 1))
	}

	host = runtimeTestHost(environment)
	runtime = NewRuntime(host, RuntimeOptions{})
	castID, err = runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil || runtime.Cancel(castID) != nil {
		t.Fatal("toggle cancel failed")
	}
	if runtime.CooldownUntil(program, 1) != 6 {
		t.Fatalf("cancel cooldown=%d", runtime.CooldownUntil(program, 1))
	}
}

func TestAmmoFailedPaymentDoesNotDecrementStock(t *testing.T) {
	policy := `{"mode":"ammo","max_stock":2,"recharge_ticks":3,"initial_stock":2}`
	program, environment := compilePolicySkill(t, "ammo", policy, 0)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 4}})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("payment = %v", err)
	}
	stock, maximum, _ := runtime.SkillStock(program, 1)
	if stock != 2 || maximum != 2 {
		t.Fatalf("stock changed after payment failure: %d/%d", stock, maximum)
	}
}

func TestCastReferencesExposeLivePolicyState(t *testing.T) {
	policy := `{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":true}`
	program, environment := compilePolicySkill(t, "charge", policy, 0)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil || runtime.Advance(5) != nil {
		t.Fatal("charge setup failed")
	}
	cast := runtime.casts[castID]
	checks := []struct {
		name string
		want any
	}{
		{"$cast.mode", "charge"},
		{"$cast.elapsed_ticks", int64(5)},
		{"$cast.charge_bp", int64(5000)},
	}
	for _, check := range checks {
		value, evalErr := runtime.evalReference(cast, referenceProgramValue{kind: referenceBuiltin, builtin: check.name})
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		if text, ok := check.want.(string); ok {
			got, _ := value.String()
			if got != text {
				t.Fatalf("%s=%q", check.name, got)
			}
		} else {
			got, _ := value.Int()
			if got != check.want.(int64) {
				t.Fatalf("%s=%d", check.name, got)
			}
		}
	}
	if err := runtime.Release(castID); err != nil {
		t.Fatal(err)
	}
	value, err := runtime.evalReference(cast, referenceProgramValue{kind: referenceBuiltin, builtin: "$cast.release_reason"})
	if err != nil {
		t.Fatal(err)
	}
	reason, _ := value.String()
	if reason != "input_release" {
		t.Fatalf("release reason=%q", reason)
	}
}

func compilePolicySkill(t *testing.T, id, policy string, cooldown Tick) (*Program, CompileEnvironment) {
	t.Helper()
	costs := `[]`
	if id == "charge" || id == "ammo" {
		costs = `[{"resource":"mana","amount":5}]`
	}
	enter := `{"flow":"effect","effect":{"type":"set_memory","name":"entered","value":true}}`
	if id == "ammo" {
		enter = `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"set_memory","name":"entered","value":true}},{"flow":"finish"}]}`
	}
	json := `{"schema":"roost.skill/v2","id":"skill.test.policy.` + id + `","name":"Policy","description":"Policy runtime.","activation":{"type":"active","policy":` + policy + `},"input_schema":{"type":"entity"},"cooldown_ticks":` + tickString(cooldown) + `,"costs":` + costs + `,"memory":{"entered":{"type":"bool","default":false}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"pulse":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},"release":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	return compileRuntimeJSON(t, json)
}

func tickString(value Tick) string {
	return strconv.FormatInt(int64(value), 10)
}
