package roost

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/cfggen"
)

func TestHelpCatalogIsCompleteAndUnique(t *testing.T) {
	if err := validateHelpTopics(); err != nil {
		t.Fatal(err)
	}
	names := helpTopicNames()
	for _, required := range []string{
		"environment", "beginner", "project", "versions", "framework-release", "generate", "add", "mods", "dao", "entity", "nest",
		"access", "transport", "lifecycle", "endpoint", "skill", "protocol", "cfggen", "tablegen", "attribute", "webroute",
		"errcode", "saga", "replication", "config", "id", "format", "deploy",
	} {
		if !contains(names, required) {
			t.Errorf("help catalog missing %q: %v", required, names)
		}
	}
}

func TestHelpOverviewListsCapabilities(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"help"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, topic := range helpTopics {
		if !strings.Contains(text, topic.Name) || !strings.Contains(text, topic.Summary) {
			t.Errorf("overview missing %s: %s", topic.Name, text)
		}
	}
	for _, want := range []string{"roost help <能力>", "roost help all", "roost help beginner"} {
		if !strings.Contains(text, want) {
			t.Errorf("overview missing %q: %s", want, text)
		}
	}
}

func TestEveryCapabilityHelpContainsConfigurationAndExample(t *testing.T) {
	for _, topic := range helpTopics {
		t.Run(topic.Name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Run([]string{"help", topic.Name}, &output, &output); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			for _, want := range []string{topic.Name + " - ", "用途:", "命令:", "配置/标记:", "示例:", indentHelp(topic.Usage), indentHelp(topic.Configuration), indentHelp(topic.Example)} {
				if !strings.Contains(text, want) {
					t.Errorf("topic output missing %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestHelpAliasesAndContextCommands(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"help", "table"}, want: "tablegen - "},
		{args: []string{"help", "roost-up"}, want: "versions - "},
		{args: []string{"help", "codegen-up"}, want: "versions - "},
		{args: []string{"help", "project-upgrade"}, want: "project - "},
		{args: []string{"project", "--help"}, want: "project - "},
		{args: []string{"project", "upgrade", "--help"}, want: "project - "},
		{args: []string{"project", "deps", "--help"}, want: "versions - "},
		{args: []string{"project", "next", "--help"}, want: "project - "},
		{args: []string{"framework", "verify", "--help"}, want: "framework-release - "},
		{args: []string{"config", "check", "-h"}, want: "config - "},
		{args: []string{"config", "enable", "player-tcp", "--help"}, want: "config - "},
		{args: []string{"id", "next", "--help"}, want: "id - "},
		{args: []string{"add", "dao", "--help"}, want: "dao - "},
	}
	for _, test := range tests {
		var output bytes.Buffer
		if err := Run(test.args, &output, &output); err != nil {
			t.Fatalf("Run(%v): %v", test.args, err)
		}
		if !strings.Contains(output.String(), test.want) {
			t.Errorf("Run(%v) missing %q:\n%s", test.args, test.want, output.String())
		}
	}
}

func TestUnknownHelpCapabilityFailsWithDiscoveryHint(t *testing.T) {
	err := Run([]string{"help", "missing"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "roost help") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeNextIDArgsAcceptsFlagsBeforeOrAfterKind(t *testing.T) {
	for _, test := range []struct {
		input []string
		want  []string
	}{
		{input: []string{"protocol"}, want: []string{"protocol"}},
		{input: []string{"-group", "game", "protocol"}, want: []string{"-group", "game", "protocol"}},
		{input: []string{"protocol", "-group", "game"}, want: []string{"-group", "game", "protocol"}},
	} {
		got := normalizeNextIDArgs(test.input)
		if strings.Join(got, "|") != strings.Join(test.want, "|") {
			t.Errorf("normalizeNextIDArgs(%v) = %v, want %v", test.input, got, test.want)
		}
	}
}

// self_sync 只在 roost.yaml 里打开，没有 add 命令；内置帮助是新手唯一不用翻仓库就能找到它的地方，
// 并且要给出生成工程里打得开的文档地址（core 文档不在生成工程里）。
func TestHelpAccessExplainsSelfSync(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"help", "access"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"access.player.self_sync", selfSyncDocumentURL} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("help access does not mention %q:\n%s", want, output.String())
		}
	}
}

func TestHelpAllPrintsEveryTopic(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"help", "all"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	for _, topic := range helpTopics {
		if !strings.Contains(output.String(), topic.Name+" - "+topic.Summary) {
			t.Errorf("help all missing %s", topic.Name)
		}
	}
}

// `roost help cfggen` 给出的命令是用户照抄的入口：按帮助里的 -out / -pkg 在生成工程里跑 cfggen 之后，roost generate 照常成功，
// 输出目录里没有别的生成器声明同名入口（不能与 tablegen 的 configs/generated 撞包）。

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
