package cfggen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20260930-CG-13：生成函数/import 与 bean 共用 Go 命名空间；
// 冲突必须在写输出前报错，不能把 gofmt 的成功当编译成功。
func TestCfggenRejectsReservedBeanNamesBeforeWriting(t *testing.T) {
	for _, name := range []string{"RegisterConfigData", "RegisterGeneratedConfigData", "MustRegisterGeneratedConfigData", "configdata", "strconv"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			meta := "package: cfg\nbeans:\n  - name: " + name + "\n    fields:\n      - { name: value, type: int32 }\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: camp, type: int32, index: true }\n"
			metaPath := filepath.Join(root, "schema.yaml")
			if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(root, "cfg")
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(out, generatedFileName)
			before := []byte("existing output must survive validation failure\n")
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := Run([]string{"-meta", metaPath, "-out", out}, &stdout)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("reserved bean %s was not rejected with a collision diagnostic: %v", name, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("invalid schema replaced output: %v\n%s", err, after)
			}
		})
	}
}

func TestCfggenNonCollidingBeanNamesCompile(t *testing.T) {
	for _, name := range []string{"Configdata", "RegisterConfigDataBean", "strconv"} {
		t.Run(name, func(t *testing.T) {
			source, err := runCfggen(t, "package: cfg\nbeans:\n  - name: "+name+"\n    fields:\n      - { name: value, type: int32 }\n")
			if err != nil {
				t.Fatal(err)
			}
			assertGeneratedConsumerCompiles(t, source)
		})
	}
}
