package cfggen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// RR-20260930-22：显式关闭索引也必须生成可编译的消费包；
// 旧实现只移除了标签/accessor，却留下未使用的 strconv import。
func TestCfggenDisabledIndexesCompile(t *testing.T) {
	var disabled strings.Builder
	for i, typ := range []string{"int32", "int64", "uint32", "uint64", "float32", "float64", "bool", "string"} {
		fmt.Fprintf(&disabled, "      - { name: value_%d, type: %s, index: false }\n", i, typ)
	}
	for name, fields := range map[string]string{
		"all disabled":                    disabled.String(),
		"no indexes":                      "      - { name: camp, type: int32 }\n",
		"string enabled numeric disabled": "      - { name: camp, type: int32, index: false }\n      - { name: name, type: string, index: true }\n",
		"mixed enabled disabled":          "      - { name: camp, type: int32, index: false }\n      - { name: flag, type: bool, index: true }\n      - { name: owner, type: uint64, index: owner_id }\n",
	} {
		t.Run(name, func(t *testing.T) {
			source, err := runCfggen(t, "package: cfg\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n"+fields)
			if err != nil {
				t.Fatal(err)
			}
			assertGeneratedConsumerCompiles(t, source)
		})
	}
}

// Compile against the real configdata package, not a generated-text assertion
// or a fake type importer. Module/cache/proxy configuration is inherited.
func assertGeneratedConsumerCompiles(t *testing.T, source string) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mod := "module example.com/cfggenconsumer\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.18.0\n\nreplace github.com/tjbdwanghaibo/roost-core => " + strconv.Quote(filepath.ToSlash(repo)) + "\n"
	for name, body := range map[string]string{"go.mod": mod, "cfg_gen.go": source} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "test", "-mod=mod", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated consumer does not compile: %v\n%s", err, out)
	}
}
