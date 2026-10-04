package roost

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RR-20260921-05：codegen/scripts/*-runtime.sh 编译生成物所用的 roost-core 版本，必须能从仓里读出来，
// 并且就是生成器自己承诺的下限 minimumVersions.Core。
//
// 旧行为：四个守卫从 scripts/source-head-check.sh 里 sed 出 core_pin="${ROOST_CORE_PIN:-vX}"；
// 6d04aea4（合仓后发布链收拢）删掉了那一行，没设 ROOST_CORE_PIN 时四个脚本全部
// "cannot determine the roost-core pin" 并 exit 2。现在它们统一调 scripts/core-pin.sh，
// 这条测试把那个脚本的输出钉在 minimumVersions 上：manifest.go 的写法变了、脚本读不出来，
// 在这里红，而不是在 CI 的守卫里 exit 2。
func TestCorePinScriptPrintsTheGeneratorMinimum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the runtime guards are POSIX sh scripts run on Linux CI")
	}
	script := filepath.Join("..", "..", "scripts", "core-pin.sh")
	cmd := exec.Command("sh", script)
	// The override would mask exactly what this test reads.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ROOST_CORE_PIN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s: %v\n%s", script, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != minimumVersions.Core {
		t.Fatalf("core-pin.sh printed %q, want minimumVersions.Core %q: the runtime guards would compile the generator's output against a framework it does not promise", got, minimumVersions.Core)
	}

	// The override still wins, for running a guard against another release.
	cmd = exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "ROOST_CORE_PIN=v9.9.9")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s with ROOST_CORE_PIN: %v\n%s", script, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "v9.9.9" {
		t.Fatalf("core-pin.sh ignored ROOST_CORE_PIN: printed %q", got)
	}
}
