package roost

// RR-20261005-NC-72：`roost help cfggen` 给出的命令是用户照抄的入口。旧 Usage 写
// `-out ./configs/generated -pkg generated`，而默认 features 含 config 的工程里 configs/generated
// 正是 tablegen（roost generate 的 config-go 步骤）的输出目录：cfggen 的 cfg_gen.go 与
// gen_table_config.go 同包，都声明 RegisterGeneratedConfigData / RegisterConfigData，包编译失败，
// roost generate 的 registry 步骤报 “RegisterConfigData is marked twice”。`roost next` 给的是
// `-out configs/cfg`，两处入口互相矛盾。承诺：按帮助里的 -out / -pkg 在生成工程里跑 cfggen 之后，
// roost generate 照常成功，输出目录里没有别的生成器声明同名入口。

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/cfggen"
)

func helpFlagValue(fields []string, name string) string {
	for i, field := range fields {
		if field == name && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func TestCfggenHelpUsageCoexistsWithTheProjectGenerators(t *testing.T) {
	t.Parallel()
	topic, ok := findHelpTopic("cfggen")
	if !ok {
		t.Fatal("roost help has no cfggen topic")
	}
	fields := strings.Fields(topic.Usage)
	out, pkg := helpFlagValue(fields, "-out"), helpFlagValue(fields, "-pkg")
	if out == "" || pkg == "" {
		t.Fatalf("cfggen usage does not name -out and -pkg: %s", topic.Usage)
	}
	root := copyOfNewProject(t, "configdata")
	meta := filepath.Join(root, "configs", "schema", "cfg.yaml")
	if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("package: "+pkg+"\ntables:\n  - name: monster\n    key: id\n    fields:\n      - {name: id, type: int32}\n      - {name: scene_id, type: int32, index: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(out, "./")))
	if err := cfggen.Run([]string{"-meta", meta, "-out", outDir, "-pkg", pkg}, io.Discard); err != nil {
		t.Fatalf("cfggen as roost help shows it (-out %s -pkg %s): %v", out, pkg, err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("roost generate after following roost help cfggen (-out %s): %v", out, err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "cfg_gen.go" || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(outDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, entryPoint := range []string{"func RegisterGeneratedConfigData(", "func RegisterConfigData("} {
			if strings.Contains(string(raw), entryPoint) {
				t.Errorf("%s/%s also declares %s: the documented -out shares a package with another generator", out, entry.Name(), strings.TrimSuffix(strings.TrimPrefix(entryPoint, "func "), "("))
			}
		}
	}
}
