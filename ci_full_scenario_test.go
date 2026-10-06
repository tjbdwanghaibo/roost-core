package roostcore_test

// A11（收尾第 2 批，2026-10-06）：framework-compat 的 full 场景在生成工程上执行的 add 序列只有一份
// （codegen/scripts/full-scenario-adds.sh），CI 与本地镜像 codegen/scripts/source-head-check.sh 都调用它，
// 而且任何一步失败都让调用方失败。
//
// 旧行为：两边各写一份。本地脚本只 add 了 access / transport / skill / saga 四步（CI 还有 component、
// dao、handler、protocol、endpoint、rpc 与 project sync），并用 `(...) || true` 吞掉失败，所以本地
// “source-head full OK” 既没跑 CI 跑的那些步骤，也不说明 add 本身成功了。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fullScenarioAdds = "codegen/scripts/full-scenario-adds.sh"

// codeLines drops full-line comments: prose naming a command is not a step that runs it.
func codeLines(raw []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed(t *testing.T) {
	directAdd := regexp.MustCompile(`roost"?\s+add\s`)
	swallowed := regexp.MustCompile(`\)\s*\|\|\s*true`)
	for _, caller := range []string{".github/workflows/framework-compat.yml", "codegen/scripts/source-head-check.sh"} {
		raw, err := os.ReadFile(filepath.FromSlash(caller))
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		for _, line := range codeLines(raw) {
			if directAdd.MatchString(line) {
				t.Errorf("%s runs an add itself (%q); the full-scenario add sequence belongs in %s", caller, strings.TrimSpace(line), fullScenarioAdds)
			}
			if strings.Contains(line, fullScenarioAdds) {
				calls++
				if strings.Contains(line, "||") {
					t.Errorf("%s ignores a failure of %s: %q", caller, fullScenarioAdds, strings.TrimSpace(line))
				}
			}
			if swallowed.MatchString(line) {
				t.Errorf("%s swallows a failing subshell: %q", caller, strings.TrimSpace(line))
			}
		}
		if calls != 1 {
			t.Errorf("%s calls %s %d times, want once", caller, fullScenarioAdds, calls)
		}
	}
	raw, err := os.ReadFile(filepath.FromSlash(fullScenarioAdds))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "set -euo pipefail") {
		t.Errorf("%s does not stop at the first failing step (set -euo pipefail)", fullScenarioAdds)
	}
	for _, line := range codeLines(raw) {
		if strings.Contains(line, "|| true") {
			t.Errorf("%s ignores a failure: %q", fullScenarioAdds, strings.TrimSpace(line))
		}
	}
}
