package roost

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 入口配置用 config.includes 叠加（App 启动时的规则，internal/configfile）：roost config check 与 doctor 按叠加后的
// 结果检查，与进程 --check-config 看到的是同一份配置；没有声明的键点名它来自哪个文件。修前工具只读入口文件，
// 报 "sid is required"，include 里的拼写错误查不到。
func TestConfigCheckReadsTheEntryFilesIncludes(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game"}, Mods: []string{"nats"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "configs", "service")
	entry := filepath.Join(dir, "config.game.yaml")
	generated, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	// 生成的配置整份搬进 include，入口只剩 includes 与一个覆盖值。
	base := filepath.Join(dir, "layers", "game.base.yaml")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(base, string(generated))
	write(entry, "config:\n  includes:\n    - layers/game.base.yaml\nlog:\n  level: debug\n")

	check := func() (configCheckReport, error) {
		t.Helper()
		var buf bytes.Buffer
		err := Run([]string{"config", "check", "--root", root, "--service", "game", "--keys-only", "--json"}, &buf, io.Discard)
		var report configCheckReport
		if decodeErr := json.Unmarshal(buf.Bytes(), &report); decodeErr != nil {
			t.Fatalf("stdout is not one JSON report: %v\n%s", decodeErr, buf.String())
		}
		return report, err
	}
	report, err := check()
	if err != nil || report.Status != "ok" {
		t.Fatalf("entry with includes: config check = %v, %+v; want ok (the keys live in the include)", err, report.Files)
	}

	write(base, strings.Replace(string(generated), "\nnats:\n", "\nnats:\n  urll: nats://x\n", 1))
	report, err = check()
	keyErrors := ""
	if len(report.Files) == 1 {
		keyErrors = strings.Join(report.Files[0].KeyErrors, "\n")
	}
	if code := configCheckExitCode(err); code != configExitKeyErrors || !strings.Contains(keyErrors, "nats.urll") || !strings.Contains(keyErrors, filepath.Join("layers", "game.base.yaml")) {
		t.Fatalf("misspelled key in the include: exit %d, key errors:\n%s\nwant exit %d naming nats.urll and layers/game.base.yaml", code, keyErrors, configExitKeyErrors)
	}
}
