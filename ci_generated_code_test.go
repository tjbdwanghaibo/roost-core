package roostcore_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// RR-20260921-05：仓内 go:generate 的产物与 codegen 的运行期守卫，都得有 CI 在跑。
//
// 旧行为：kit/service 与 service/ 下 13 处 //go:generate 的产物提交在仓里，ci.yml 没有任何一步重新生成
// 再比对——把 service/session/session_rpc_gen.go 改脏，build / vet / test 全绿，只有 go generate 能看出来
// （generate --check 只跑在生成工程那侧）。codegen/scripts/*-runtime.sh 四个守卫是 RR-20260917-05/06、
// RR-20260918-01、U-0224 唯一的运行期证明（codegen 自身的测试只比文本），合仓后取 pin 的那一行被删，
// 四个脚本默认 exit 2，也没有任何 workflow 调用它们。两件事同根：守卫在仓里，CI 不跑。
// 这两条测试把"CI 跑它们"钉在 workflow 上，删掉那一步会在普通 go test 里红，而不是再沉默一次。

// workflowStep is the part of a GitHub Actions step these checks read.
type workflowStep struct {
	Name            string `yaml:"name"`
	Run             string `yaml:"run"`
	If              string `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
}

type workflowFile struct {
	Jobs map[string]struct {
		If    string         `yaml:"if"`
		Steps []workflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// stepRef locates a step for error messages.
type stepRef struct {
	workflow, job string
	step          workflowStep
	jobIf         string
}

// workflowSteps parses every workflow and returns its steps in file order.
func workflowSteps(t *testing.T, glob string) []stepRef {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(".github", "workflows", glob))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no workflow matches %s", glob)
	}
	sort.Strings(paths)
	var steps []stepRef
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		jobs := make([]string, 0, len(wf.Jobs))
		for name := range wf.Jobs {
			jobs = append(jobs, name)
		}
		sort.Strings(jobs)
		for _, job := range jobs {
			for _, step := range wf.Jobs[job].Steps {
				steps = append(steps, stepRef{workflow: filepath.Base(path), job: job, step: step, jobIf: wf.Jobs[job].If})
			}
		}
	}
	return steps
}

// shellCommands is a step's run script without shell comment lines: a comment
// that names a command is prose, not a step that runs it.
func shellCommands(run string) []string {
	var lines []string
	for _, line := range strings.Split(run, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

// unconditional reports whether a step always runs and can fail its job: no
// `if:` on the step or its job, and no continue-on-error.
func (s stepRef) unconditional() bool {
	if s.step.If != "" || s.jobIf != "" {
		return false
	}
	switch v := s.step.ContinueOnError.(type) {
	case nil:
		return true
	case bool:
		return !v
	default:
		return false
	}
}

// ci.yml must regenerate everything and fail when that changes the tree, in
// ONE step: a diff in a later step can be dropped or reordered on its own, and
// a generate without a diff is a step that always passes.
func TestCIRegeneratesTheCommittedGeneratedCode(t *testing.T) {
	var found []string
	for _, ref := range workflowSteps(t, "ci.yml") {
		commands := shellCommands(ref.step.Run)
		generateAt := -1
		for i, line := range commands {
			if strings.Contains(line, "go generate ./...") {
				generateAt = i
				break
			}
		}
		if generateAt < 0 {
			continue
		}
		checked := false
		for _, line := range commands[generateAt+1:] {
			if strings.Contains(line, "git status --porcelain") || strings.Contains(line, "git diff --exit-code") {
				checked = true
			}
		}
		where := ref.job + " / " + ref.step.Name
		switch {
		case !checked:
			t.Errorf("ci.yml %s runs `go generate ./...` but never checks the tree afterwards (git status --porcelain / git diff --exit-code); the step passes whatever generate changed", where)
		case !ref.unconditional():
			t.Errorf("ci.yml %s is conditional or continue-on-error; a stale generated file must fail every run", where)
		default:
			found = append(found, where)
		}
	}
	if len(found) == 0 {
		t.Error("no ci.yml step runs `go generate ./...` and then fails on a dirty tree: a hand-edited or stale *_gen.go (service/session/session_rpc_gen.go was the probe) builds, vets and tests green, and nothing in CI regenerates it (RR-20260921-05)")
	}
}

// Every codegen runtime guard must be run by some workflow, unconditionally.
// The scripts are the only place the generators' output meets the real
// runtime; a guard nobody runs is how they stayed broken from 6d04aea4 to
// the 10-04 triage.
func TestEveryCodegenRuntimeGuardRunsInSomeWorkflow(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("codegen", "scripts", "*-runtime.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) == 0 {
		t.Fatal("codegen/scripts has no *-runtime.sh; the guards moved or were renamed, update this test with them")
	}
	steps := workflowSteps(t, "*.yml")
	for _, script := range scripts {
		want := filepath.ToSlash(script)
		ran := false
		for _, ref := range steps {
			if !ref.unconditional() {
				continue
			}
			for _, line := range shellCommands(ref.step.Run) {
				if strings.Contains(line, want) {
					ran = true
				}
			}
		}
		if !ran {
			t.Errorf("no workflow step runs %s unconditionally; it is the only runtime proof of what the generator emits, and codegen's own tests only compare text (RR-20260921-05)", want)
		}
	}
}
