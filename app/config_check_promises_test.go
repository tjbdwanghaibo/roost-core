package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20261007-01（F01-8）：配置检查必须检查真实文件；开发启动允许缺省文件缺失，不代表检查命令可以宣称文件有效。
func TestCheckConfigRequiresAnExistingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			a := newTestApp(t, &errService{})
			var out bytes.Buffer
			a.RootCmd().SetOut(&out)
			args := []string{"game", "--check-config"}
			if explicit {
				args = append(args, "--config", "missing.yaml")
			}
			a.RootCmd().SetArgs(args)
			err := a.Execute()
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing config: error = %v, want os.ErrNotExist; output = %q", err, out.String())
			}
			if strings.Contains(out.String(), "config ok:") {
				t.Fatalf("missing file reported as valid: %q", out.String())
			}
		})
	}
}

func TestCheckConfigAcceptsExistingDefaultFileWithoutStartingService(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join("configs", "service", "config.game.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("sid: 1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, &errService{}) // Serve 一旦被误调用就返回 errServeFailed。
	var out bytes.Buffer
	a.RootCmd().SetOut(&out)
	a.RootCmd().SetArgs([]string{"game", "--check-config"})
	if err := a.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "config ok:") {
		t.Fatalf("valid file not reported: %q", out.String())
	}
}

// RR-20261007-02（F01-9）：服务 Mod 与共享 Mod 使用同一名字空间，外部依赖可以引用共享 Mod，但不能覆盖它。
func TestSortModsRejectsNameAlreadyOwnedBySharedMod(t *testing.T) {
	external := map[ModName]struct{}{"shared": {}}
	if _, err := sortMods([]Mod{&orderedTestMod{name: "shared"}}, external); err == nil {
		t.Fatal("service mod reused a shared mod name without rejection")
	}
	consumer := &orderedTestMod{name: "consumer", hard: []ModName{"shared"}}
	if got, err := sortMods([]Mod{consumer}, external); err != nil || len(got) != 1 || got[0] != consumer {
		t.Fatalf("legitimate shared dependency rejected: mods=%v err=%v", got, err)
	}
}
