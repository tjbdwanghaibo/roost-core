package main

// A1 盲区（维护者决定，2026-10-06 第十三轮）：A1 要求事务里会改的状态放在 DAO，由 DAO 的回滚统一兜住。
// 组件把可变状态放在普通字段里、也不登记 undo 时，事务失败这些字段静默不回滚；旧 glsvet 只看组件的
// undo 登记，对这种组件什么都不报。承诺：组件方法（初始化钩子除外）写非 DAO、非 //roost:cache、
// 非函数类型的字段时打印 hint:（含经同包 helper 写的，跟进一层），只提示、不计入违例、退出码 0；
// 缓存字段、函数类型字段、DAO 句柄和 OnInitFinish 里的写不提示。

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestComponentFieldWritesOutsideTheDaoAreHinted(t *testing.T) {
	code, output := runGlsvet(t, "testdata/a1fields")
	if code != 0 {
		t.Fatalf("exit %d, want 0: field-write hints never fail the run; output:\n%s", code, output)
	}
	want := []string{
		"quest.go:31:45: hint: component QuestComponent.Accept writes field active outside the DAO",
		"quest.go:34:2: hint: component QuestComponent.Finish writes field active outside the DAO",
		"quest.go:35:2: hint: component QuestComponent.Finish writes field history outside the DAO",
		"quest.go:41:38: hint: component QuestComponent.Advance writes field progress outside the DAO",
		"quest.go:43:36: hint: component QuestComponent.Reset writes quests.progress outside the DAO through resetQuests",
	}
	for _, line := range want {
		if !strings.Contains(output, line) {
			t.Errorf("missing hint %q", line)
		}
	}
	if got := strings.Count(output, "outside the DAO"); got != len(want) {
		t.Errorf("%d field-write hints, want %d (no hint for the //roost:cache fields, the func field, the DAO handle, OnInitFinish or reads)", got, len(want))
	}
	for _, quiet := range []string{"field lookup", "field byTag", "field onDone", "field dao", "OnInitFinish", ".Level", ".index"} {
		if strings.Contains(output, quiet) {
			t.Errorf("unexpected hint mentioning %q", quiet)
		}
	}
	if t.Failed() {
		t.Logf("glsvet output:\n%s", output)
	}
}

// 真实组件上的误报回归：combatcomponent.CombatComponent 的 projection 是函数类型字段，由装配方法
// ProjectAttributes 装上（业务的属性投影，不是事务状态），不提示；它的状态全在 CombatDao 里。
// 规则只按字段类型名认 DAO 句柄（dao *CombatDao），这里同时钉住这一点。
func TestSkillPackagesGetNoComponentFieldHint(t *testing.T) {
	for _, directory := range []string{"../../gameplay/skill", "../../gameplay/skill/combatcomponent", "../../gameplay/skill/combat", "../../gameplay/skill/skillsync"} {
		fileSet := token.NewFileSet()
		packages, err := parser.ParseDir(fileSet, directory, func(info os.FileInfo) bool {
			return !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if len(packages) == 0 {
			t.Fatalf("%s: no package parsed", directory)
		}
		for _, pkg := range packages {
			if hints := componentFieldHints(fileSet, pkg); len(hints) != 0 {
				t.Fatalf("%s: unexpected A1 field-write hints %q", directory, hints)
			}
		}
	}
}

// 字段写提示要读 //roost:cache，vetDirectory 因此改为带注释解析。顺带生效的是 isNestHandler 读函数文档里的
// roost:nest：之前按 0 解析、Doc 恒为 nil，只有名字以 handler 开头的函数才被当作 Nest handler 检查。
func TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix(t *testing.T) {
	if findings := vetSource(t, `package handler
//roost:nest rollback=undo durability=strict
func moveTo() { go func() {}() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1: a //roost:nest function is a Nest handler whatever its name", findings)
	}
}
