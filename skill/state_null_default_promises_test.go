package skill

import (
	"errors"
	"strings"
	"testing"
)

// RR-20261006-02：默认值为 null 的实体持久状态，作者用 modify_state set 写入实体是合法写法（编译器接受，
// state 声明类型就是 entity，null 只表示“还没写过”）。修前 Runtime 把 null 默认值按字面量求值成
// Base=valueKindNull 的缺省值交给 Host，MemoryHost.applyStateOperation 比较 before.typ.Base != operand.typ.Base，
// 于是第一次 set（状态还不存在、before 取默认值）每次都报 ErrRuntimeTypeMismatch。承诺：Runtime 交给 Host 的
// Default 总是带 state 声明的类型；第一次 set 成功，状态里存的是写入的实体，事件里的 Before 是该类型的缺省值，
// 再次施法时 read_state 读回写入的实体。
func TestNullDefaultEntityStateCanBeSet(t *testing.T) {
	state := `{"mark":{"type":"entity","scope":"owner","default":null,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}}`
	// 读到 mark 已写入时打伤目标（第二次施法才会读到），再 set 为本次目标。
	flow := `{"flow":"sequence","steps":[` +
		`{"flow":"if","condition":{"op":"exists","args":[{"read_state":{"state":"mark","owner":"$caster","snapshot":"current"}}]},"then":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}}},` +
		`{"flow":"effect","effect":{"type":"modify_state","state":"mark","owner":"$caster","operation":"set","value":"$input.target","duration_ticks":20,"expiry_policy":"refresh"}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(state), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)
	json = strings.Replace(json, `{"flow":"finish"}`, flow, 1)
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	_, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if errors.Is(err, ErrRuntimeTypeMismatch) {
		t.Fatalf("modify_state set on a null-default entity state reported a type mismatch: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var changed *StateChangeEvent
	for _, event := range host.Events(0) {
		if event.Kind == "state_changed" {
			changed = event.State
		}
	}
	if changed == nil {
		t.Fatal("no state_changed event")
	}
	if entity, ok := changed.After.Entity(); !ok || entity != 2 {
		t.Fatalf("after=%#v", changed.After)
	}
	if changed.Before.Present() || changed.Before.Type().Base != valueKindEntity {
		t.Fatalf("before should be the missing value of the declared entity type, got %#v", changed.Before)
	}
	snapshot := host.SkillPersistentStateSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("persistent state=%#v", snapshot)
	}
	if entity, ok := snapshot[0].Value.Entity(); !ok || entity != 2 {
		t.Fatalf("persistent state=%#v", snapshot)
	}
	if host.HealthForTest(2) != 100 {
		t.Fatalf("first cast must not read a mark, health=%d", host.HealthForTest(2))
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 98 {
		t.Fatalf("second cast should read the mark and damage the target, health=%d", host.HealthForTest(2))
	}
}
