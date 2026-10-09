package skill

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 生成工程在启动期对 game/skills/*.json 做 Parse + Compile，任何 error 诊断都让
// game 服起不来。生成器写出的两份定义——`roost add skill` 的骨架与 game-demo 的
// fireball——此前没有任何测试走这条编译链（RR-20261005-NC-151 收紧编译规则时
// 就可能把它们变成启动失败）。core 不能 import codegen，这里把它们当数据读：
// 骨架取自 add_skill.go 里 Sprintf 的格式串，fireball 取自 demo 模板。
func TestGeneratedSkillDefinitionsCompileWithoutDiagnostics(t *testing.T) {
	definitions := map[string]string{
		"roost add skill skeleton": addSkillSkeleton(t, "planet", "fire_ball", "FireBall"),
		"game-demo fireball":       demoFireball(t, "planet"),
	}
	for name, input := range definitions {
		t.Run(name, func(t *testing.T) {
			definition, err := Parse([]byte(input))
			if err != nil {
				t.Fatalf("Parse: %v\n%s", err, input)
			}
			program, diagnostics := Compile(definition, DefaultCompileEnvironment())
			if program == nil || len(diagnostics) != 0 {
				t.Fatalf("Compile: program=%v diagnostics=%#v", program != nil, diagnostics)
			}
		})
	}
}

func addSkillSkeleton(t *testing.T, project, snake, pascal string) string {
	t.Helper()
	path := filepath.Join("..", "..", "codegen", "internal", "roost", "add_skill.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var format string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || format != "" {
			return format == ""
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(call.Args) == 0 {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING || !strings.Contains(literal.Value, "roost.skill/v2") {
			return true
		}
		format, err = strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		return false
	})
	if format == "" {
		t.Fatalf("no Skill skeleton format string in %s", path)
	}
	return fmt.Sprintf(format, project, snake, pascal)
}

func demoFireball(t *testing.T, project string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "demo", "game", "skills", "fireball.json.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "{{PROJECT}}", project)
}
