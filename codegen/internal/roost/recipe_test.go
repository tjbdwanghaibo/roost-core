package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRecipeSelectionRejectsUnknownAndConflictingTemplates(t *testing.T) {
	for _, options := range []NewOptions{
		{Name: "bad", Module: "example.com/bad", Recipe: "missing"},
		{Name: "bad", Module: "example.com/bad", Recipe: "casual", Template: "game-demo"},
	} {
		options.Out = filepath.Join(t.TempDir(), "must-not-exist")
		if _, _, err := NewProject(options); err == nil {
			t.Fatal("invalid recipe accepted")
		}
		if _, err := os.Stat(options.Out); !os.IsNotExist(err) {
			t.Fatalf("partial project published: %v", err)
		}
	}
}

// 整条命令用自定义 module / service 生成，不依赖 demo 的固定路径；检查模块与配方自己的场景、生成漂移。
func TestRecipesCreateBuildableCompositions(t *testing.T) {
	if testing.Short() {
		t.Skip("generates and runs both recipe projects")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"casual", "turn-pvp"} {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "starter")
			var out bytes.Buffer
			if err := Run([]string{"project", "new", "starter", "-module", "example.com/studio/starter", "-services", "arena", "-recipe", name, "-out", target, "-skip-deps"}, &out, &out); err != nil {
				t.Fatalf("%s: %v\n%s", name, err, &out)
			}
			modFile := filepath.Join(target, "go.mod")
			raw, err := os.ReadFile(modFile)
			if err != nil {
				t.Fatal(err)
			}
			raw = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(raw, nil)
			raw = append(raw, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
			if err := os.WriteFile(modFile, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			manifest, err := LoadManifest(target)
			if err != nil {
				t.Fatal(err)
			}
			recipes, err := loadRecipes()
			if err != nil {
				t.Fatal(err)
			}
			for _, module := range recipes[name].Modules {
				if installed, ok := manifest.Modules[module]; !ok || installed.Service != "arena" {
					t.Fatalf("%s: missing %s on arena", name, module)
				}
			}
			if !manifest.Access["player"].SelfSync {
				t.Fatal("recipe omitted self-sync")
			}
			for _, args := range [][]string{{"test", "-json", "-count=1", "./..."}, {"test", "-json", "-race", "-count=1", "-run", "^TestRecipe|^TestEnter", "./game/handler", "./game/entities/player", "./internal/access/player/tcp"}, {"build", "./..."}, {"vet", "./..."}} {
				cmd := exec.Command("go", args...)
				cmd.Dir, cmd.Env = target, generatedProjectGoEnv()
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("recipe %s go %s: %v\n%s", name, strings.Join(args, " "), err, output)
				}
				if args[0] == "test" {
					var passed, skipped int
					for _, line := range bytes.Split(output, []byte("\n")) {
						var event struct{ Action, Test string }
						if json.Unmarshal(line, &event) == nil && strings.HasPrefix(event.Test, "TestRecipe") {
							if event.Action == "pass" {
								passed++
							}
							if event.Action == "skip" {
								skipped++
							}
						}
					}
					want := map[string]int{"casual": 9, "turn-pvp": 7}[name]
					if passed != want || skipped != 0 {
						t.Fatalf("recipe %s scenarios: %d passed, %d skipped, want %d/0", name, passed, skipped, want)
					}
					t.Logf("recipe %s: %d integration/auth scenarios, zero skips", name, passed)
				}
			}
			if _, err := VerifyModules(context.Background(), target, nil, nil); err != nil {
				t.Fatalf("recipe %s verify: %v", name, err)
			}
			if name == "casual" {
				recipeMutationMustFail(t, target, "game/entities/player/enter_component.go", "if err := runFirstEnter(component.owner, nowUnixMilli); err != nil {", "if err := error(nil); err != nil {", "./game/handler", "TestRecipeFirstGrantAndRepeatEnter", "initial coins = 0")
				recipeMutationMustFail(t, target, "game/entities/player/enter_component.go", "if state.GetInitialized() {", "if false && state.GetInitialized() {", "./game/handler", "TestRecipeFirstGrantAndRepeatEnter", "repeat =")
				recipeMutationMustFail(t, target, "internal/access/player/tcp/auth.go", "principal.ServerID != bound.sid || ", "", "./internal/access/player/tcp", "TestRecipeAuthenticationRejectsWrongServer", "wrong server:")
			}
			// 同步后仍保持业务文件；再生不应改变框架生成物。
			rule := filepath.Join(target, "RECIPE.md")
			text, err := os.ReadFile(rule)
			if err != nil {
				t.Fatal(err)
			}
			text = append(text, []byte("\nBusiness-owned addition.\n")...)
			if err := os.WriteFile(rule, text, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := SyncProjectContext(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(rule)
			if err != nil || !bytes.Equal(text, after) {
				t.Fatalf("sync changed recipe business file: %v", err)
			}
			if err := Generate(target, GenerateOptions{Check: true}); err != nil {
				t.Fatalf("recipe %s drift: %v", name, err)
			}
		})
	}
}

// 只接受行为断言变红；无法编译或起不来的变异不算测试有效。
func recipeMutationMustFail(t *testing.T, root, rel, from, to, pkg, test, evidence string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(before), from) != 1 {
		t.Fatalf("mutation target changed: %s", rel)
	}
	defer func() {
		if err := os.WriteFile(path, before, 0o644); err != nil {
			t.Error(err)
		}
	}()
	if err := os.WriteFile(path, []byte(strings.Replace(string(before), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "^"+test+"$", pkg)
	cmd.Dir, cmd.Env = root, generatedProjectGoEnv()
	out, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte("--- FAIL: "+test)) || !bytes.Contains(out, []byte(evidence)) {
		t.Fatalf("mutation not caught by expected behavior %s: %v\n%s", test, err, out)
	}
	t.Logf("caught mutation in %s: %s", rel, evidence)
}

// --skip-deps 用于尚未发布的 core + 后续本地 replace；配方安装不得提前执行 go mod tidy。
func TestRecipeSkipDepsDefersDependencyCommands(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var output bytes.Buffer
	target := filepath.Join(t.TempDir(), "offline")
	err := Run([]string{"project", "new", "offline", "-module", "example.com/offline", "-recipe", "casual", "-out", target, "-skip-deps"}, &output, &output)
	if err != nil {
		t.Fatalf("skip-deps executed a dependency command: %v\n%s", err, &output)
	}
}
