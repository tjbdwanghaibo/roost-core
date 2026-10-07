package skill

import (
	"encoding/json"
	"testing"
)

// 正式创建的 entity 衍生物跨 goto 存活，finish 后移交，不伪造 phase/cast 作用域。
func TestEntityCarrySurvivesGotoAndNormalFinish(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"carry":{"target":"$caster"},"completion":{"type":"end"}}}},{"flow":"goto","phase":"next"}]}`
	var document map[string]any
	if err := json.Unmarshal([]byte(hostCapabilitySkill("carry-goto", `[]`, flow)), &document); err != nil {
		t.Fatal(err)
	}
	var next map[string]any
	if err := json.Unmarshal([]byte(`{"id":"next","timeout_ticks":0,"on":{"enter":{"flow":"wait","ticks":3,"then":{"flow":"finish"}}}}`), &next); err != nil {
		t.Fatal(err)
	}
	document["phases"] = append(document["phases"].([]any), next)
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, string(data)), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	id, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	var spawn *SpawnInstance
	runtime.spawns.each(func(value *SpawnInstance) { spawn = value }, spawnCasting)
	if spawn == nil || spawn.Scope != SpawnScopeEntity || !spawn.Motion.CarryAttached || runtime.casts[id].currentPhase != 1 {
		t.Fatalf("goto did not preserve entity carry: spawn=%+v", spawn)
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	if !spawn.handedOff || spawn.Status != SpawnRunning || !spawn.Motion.CarryAttached {
		t.Fatalf("normal finish stopped entity carry: %+v", spawn)
	}
}
