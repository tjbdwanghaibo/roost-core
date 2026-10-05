package skill

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// B3 ①（维护者 2026-10-05）：lower 里每一次“名字 → 槽位 / handle”的查找，失败时一律
// fail-fast 返回编译错误（LOWER_UNRESOLVED），不再用 map 零值兜底，也不 panic。
//
// 这是类型检查之外的第二道防线：NC-210（未声明 memory 名字落到槽位 0）、NC-214（status /
// attribute / resource 名字兜底成 handle 0）都是“前面的 pass 漏查一种名字，lower 静默
// 产出指向别的槽位的 Program”。这里模拟前面漏查：对每个种子，从刚编译好的产物里删掉某张
// 查找表的一个条目，再 lower。承诺：要么报 LOWER_UNRESOLVED 编译错误，要么产出与原来逐字段
// 相同的 Program（这个名字本来就没被 lower 用到）。静默产出不同的 Program 或 panic 都算违反。
//
// 修前红：authority 各表、damage、ability property 被删后静默兜底成 0；
// goto、state plan、temporal profile、snapshot plan 被删后 panic；$input / $memory 引用
// 查不到时退成 builtin，运行期才 ErrProgramInvariant。

type lowerLookupTable struct {
	name  string
	table func(*compileArtifacts) any // map[string]T
}

var lowerLookupTables = []lowerLookupTable{
	{"authority.attributes", func(a *compileArtifacts) any { return a.authority.attributes }},
	{"authority.resources", func(a *compileArtifacts) any { return a.authority.resources }},
	{"authority.statuses", func(a *compileArtifacts) any { return a.authority.statuses }},
	{"authority.collision", func(a *compileArtifacts) any { return a.authority.collision }},
	{"authority.tags", func(a *compileArtifacts) any { return a.authority.tags }},
	{"authority.unitTemplates", func(a *compileArtifacts) any { return a.authority.unitTemplates }},
	{"gameplay.damage", func(a *compileArtifacts) any { return a.gameplay.damage }},
	{"snapshots.reads", func(a *compileArtifacts) any { return a.snapshots.reads }},
	{"state.slots", func(a *compileArtifacts) any { return a.state.slots }},
	{"state.plans", func(a *compileArtifacts) any { return a.state.plans }},
	{"temporal.profiles", func(a *compileArtifacts) any { return a.temporal.profiles }},
	{"graph.Index", func(a *compileArtifacts) any { return a.graph.Index }},
}

func mapKeys(table any) []string {
	value := reflect.ValueOf(table)
	if value.Kind() != reflect.Map {
		return nil
	}
	keys := make([]string, 0, value.Len())
	for _, key := range value.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	return keys
}

func dropMapKey(table any, key string) {
	reflect.ValueOf(table).SetMapIndex(reflect.ValueOf(key), reflect.Value{})
}

// lowerOutcome 跑一次 lower，panic 当作结果返回而不是让测试崩掉。
func lowerOutcome(artifacts *compileArtifacts) (program *Program, diagnostics []Diagnostic, panicked any) {
	defer func() { panicked = recover() }()
	program, diagnostics = lowerProgram(artifacts)
	return program, diagnostics, nil
}

func lowerSeedArtifacts(t *testing.T, seed mutationSeed, environment CompileEnvironment) func() *compileArtifacts {
	t.Helper()
	definition, err := Parse(seed.data)
	if err != nil {
		return nil
	}
	if artifacts, diagnostics := compileToArtifacts(definition, environment); artifacts == nil || diagnosticsHaveErrors(diagnostics) {
		return nil
	}
	return func() *compileArtifacts {
		artifacts, _ := compileToArtifacts(definition, environment)
		return artifacts
	}
}

func checkLowerAfterDrop(t *testing.T, label string, baseline *Program, artifacts *compileArtifacts, mustFail bool) {
	t.Helper()
	program, diagnostics, panicked := lowerOutcome(artifacts)
	switch {
	case panicked != nil:
		t.Errorf("%s: lower panicked (%v), want a LOWER_UNRESOLVED compile error", label, panicked)
	case diagnosticsHaveErrors(diagnostics):
		if program != nil {
			t.Errorf("%s: lower reported errors but still returned a Program", label)
		}
		for _, diagnostic := range diagnostics {
			if diagnostic.Code != DiagnosticLowerUnresolved || diagnostic.Path == "" {
				t.Errorf("%s: diagnostic %+v, want LOWER_UNRESOLVED with a source path", label, diagnostic)
			}
		}
	case mustFail:
		t.Errorf("%s: lower produced a Program for an unresolved reference instead of a compile error", label)
	case !reflect.DeepEqual(program, baseline):
		t.Errorf("%s: lower silently produced a different Program instead of a compile error (zero-value fallback)", label)
	}
}

func TestLowerRefusesEveryUnresolvedLookup(t *testing.T) {
	environment := DefaultCompileEnvironment()
	checked := 0
	for _, seed := range mutationSeeds(t) {
		fresh := lowerSeedArtifacts(t, seed, environment)
		if fresh == nil {
			continue
		}
		baseline, diagnostics, panicked := lowerOutcome(fresh())
		if panicked != nil || diagnosticsHaveErrors(diagnostics) {
			t.Fatalf("%s: baseline lower failed: panic=%v diagnostics=%v", seed.name, panicked, diagnostics)
		}
		for _, table := range lowerLookupTables {
			for _, key := range mapKeys(table.table(fresh())) {
				artifacts := fresh()
				dropMapKey(table.table(artifacts), key)
				checkLowerAfterDrop(t, fmt.Sprintf("%s: drop %s[%q]", seed.name, table.name, key), baseline, artifacts, false)
				checked++
			}
		}
		// 输入槽与 memory 声明决定 Program 的布局，删掉它们 Program 必然不同；只在定义确实
		// 引用了它们时要求报错（之前查不到的引用退成 builtin，到运行期才 ErrProgramInvariant）。
		for _, name := range mapKeys(fresh().input.Slots) {
			if !strings.Contains(string(seed.data), `"`+name) {
				continue
			}
			artifacts := fresh()
			delete(artifacts.input.Slots, name)
			checkLowerAfterDrop(t, fmt.Sprintf("%s: drop input slot %q", seed.name, name), baseline, artifacts, true)
			checked++
		}
		// ability.properties 同时是 Program 的能力属性表（lowerAbilityProperties 全量导出），删掉
		// 条目 Program 必然不同；只在定义引用了这个属性时要求报错。
		for _, name := range mapKeys(fresh().ability.properties) {
			if !strings.Contains(string(seed.data), `"property":"`+name+`"`) {
				continue
			}
			artifacts := fresh()
			delete(artifacts.ability.properties, name)
			checkLowerAfterDrop(t, fmt.Sprintf("%s: drop ability property %q", seed.name, name), baseline, artifacts, true)
			checked++
		}
		for _, name := range mapKeys(fresh().ir.memory) {
			artifacts := fresh()
			delete(artifacts.ir.memory, name)
			program, diagnostics, panicked := lowerOutcome(artifacts)
			referenced := strings.Contains(string(seed.data), `"$memory.`+name) || strings.Contains(string(seed.data), `"name":"`+name+`"`)
			if panicked != nil || (referenced && !diagnosticsHaveErrors(diagnostics)) || (!referenced && program == nil && !diagnosticsHaveErrors(diagnostics)) {
				t.Errorf("%s: drop memory %q: panic=%v errors=%v, want a LOWER_UNRESOLVED compile error for a referenced memory", seed.name, name, panicked, diagnosticsHaveErrors(diagnostics))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no lookup was exercised: the seeds did not compile")
	}
	t.Logf("checked %d dropped lookups", checked)
}
