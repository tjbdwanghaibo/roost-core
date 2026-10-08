package skill

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var documentationJSONFence = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

// 开发 AI 提示词已退场；正式使用手册中的完整示例必须仍能被实际解析器和编译器接受。
// 至少有一个示例，避免删除全部代码块后得到虚假的绿色结果。
func TestUserGuideExamplesCompile(t *testing.T) {
	guide := mustReadDocumentation(t, "../docs/skill/skill.md")
	examples := documentationJSONFence.FindAllStringSubmatch(guide, -1)
	if len(examples) == 0 {
		t.Fatal("skill user guide has no complete JSON example")
	}
	for index, match := range examples {
		definition, err := Parse([]byte(match[1]))
		if err != nil {
			t.Fatalf("user guide JSON example %d does not parse: %v", index, err)
		}
		if program, diagnostics := Compile(definition, DefaultCompileEnvironment()); program == nil || diagnosticsHaveErrors(diagnostics) {
			t.Fatalf("user guide JSON example %d does not compile: %#v", index, diagnostics)
		}
	}
	if strings.Contains(guide, "{{USER_SKILL_DESCRIPTION}}") {
		t.Fatal("skill user guide still contains an AI prompt placeholder")
	}
}

func mustReadDocumentation(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
