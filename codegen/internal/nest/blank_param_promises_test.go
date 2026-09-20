package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0250：handler 的参数名写成 `_` 是合法 Go，生成物却不是。
//
// 生成的 sender 把参数名原样抄进签名，于是 `_ int64` 变成一个叫 `_` 的形参
// **被当成实参传递**：`cannot use _ as value or type`。整个工程编译不过，
// 而报错指向生成文件，跟"我把某个参数改成了 `_`"之间没有提示。
//
// 这是 RR-20260919-06 的修复过程中撞上的：那个 handler 不再需要 nowUnix，
// 按 Go 的习惯改成 `_`，生成器就塌了。

func TestABlankParameterNameGeneratesUsableCode(t *testing.T) {
	dir := t.TempDir()
	source := `package handler

import player "example.com/demo/game/entities/player"

//roost:nest rollback=undo durability=strict
func handlerGrant(target player.IBagEntity, orderID string, _ int64) (bool, error) {
	return true, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "grant.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Every generated file, the wrapper and both sender packages: a blank
	// name must never be something the generated code declares or passes.
	found := false
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_nest_gen.go") {
			return err
		}
		found = true
		generated, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, forbidden := range []string{"_ int64", "paidAtUnix, _)", ", _ "} {
			if strings.Contains(string(generated), forbidden) {
				t.Errorf("%s carries the blank parameter name (%q):\n%s", filepath.Base(path), forbidden, generated)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("nothing was generated")
	}
}
