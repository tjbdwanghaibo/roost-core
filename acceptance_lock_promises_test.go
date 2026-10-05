package roostcore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 维护者决定 A5（2026-10-05）：隔离测试环境按共享模式使用——多个会话可以并行跑各自 `-run` 选定、
// 自建代理 / 进程注入故障的用例；会改动整套环境的全局操作（dataengine-env.sh 的 up / down / heal / reset /
// fault、remote-fault.sh、故障矩阵、长稳、调用这些命令的 Go 故障用例）运行期间必须持有
// <根>/remote-acceptance.lock。脚本命令自己的持锁由 kit/scripts/integration/dataengine_env_test.sh 用垫片验证；
// 这里守调用方：
//
//   - 调用 fault / heal 的 Go 用例从 fault 到 heal 的整段持锁（holdAcceptanceLock）。旧行为：kit/dataengine 的
//     三个 failover 用例手工 `-run` 时不持锁，命令各自只检查锁空闲，fault 与 heal 之间别的会话的 heal /
//     矩阵照常进入。
//   - 调起 dataengine-env.sh / remote-fault.sh 改动环境的脚本，自己持锁或经 acquire_acceptance_lock 持锁。
func TestGlobalEnvironmentOperationsHoldTheAcceptanceLock(t *testing.T) {
	testFunc := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "testdata" || entry.Name() == "artifacts" || entry.Name() == "docs") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, "_test.go") || path == "acceptance_lock_promises_test.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(body)
		if !strings.Contains(source, "dataengine-env.sh") || !strings.Contains(source, "exec.Command") {
			return nil
		}
		bounds := testFunc.FindAllStringSubmatchIndex(source, -1)
		for i, bound := range bounds {
			end := len(source)
			if i+1 < len(bounds) {
				end = bounds[i+1][0]
			}
			fn := source[bound[0]:end]
			if (strings.Contains(fn, "runEnvironment(t, \"fault\"") || strings.Contains(fn, "healEnvironment(")) && !strings.Contains(fn, "holdAcceptanceLock(t)") {
				t.Errorf("%s: %s stops shared environment nodes / heals the environment without holding remote-acceptance.lock for the whole run; call holdAcceptanceLock(t) before registering the heal", path, source[bound[2]:bound[3]])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	invokes := regexp.MustCompile(`dataengine-env\.sh"? (up|down|heal|reset|fault)|remote-fault\.sh|ROOST_REMOTE_FAULT_SCRIPT=`)
	scripts, _ := filepath.Glob(filepath.Join("scripts", "*.sh"))
	perf, _ := filepath.Glob(filepath.Join("scripts", "perf", "*.sh"))
	for _, path := range append(scripts, perf...) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		if !invokes.MatchString(source) {
			continue
		}
		holds := strings.Contains(source, "acquire_acceptance_lock") ||
			(strings.Contains(source, `mkdir "$lock"`) && strings.Contains(source, "ROOST_REMOTE_ACCEPTANCE_LOCK_HELD"))
		if !holds {
			t.Errorf("%s changes the shared isolated environment (dataengine-env.sh / remote-fault.sh) without holding remote-acceptance.lock", path)
		}
	}
}
