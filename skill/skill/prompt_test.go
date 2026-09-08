package skill

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var promptJSONFence = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

func TestPromptContractsDescribeCanonicalSkillV2(t *testing.T) {
	system := mustReadPrompt(t, "../docs/ai-skill-system-prompt.md")
	for _, required := range []string{
		"roost.skill/v2", "Direct Root", "Skill, Phase, Flow, Select, Effect", "fixed Motion pipeline", "Numeric", "input_schema",
		"Attribute", "snapshot", "Gameplay Tag", "Gameplay Element", "CastWindow", "EventFilter", "ProcPolicy", "Area", "enter", "leave",
		"Combat Resolver", "persistent state", "ability", "status", "owned entity", "result.success", "result.failure", "$local.<name>", "temporal snapshot", "world rollback", "Visual",
	} {
		if !strings.Contains(system, required) {
			t.Fatalf("system prompt misses %q", required)
		}
	}
	user := mustReadPrompt(t, "../docs/ai-skill-user-prompt.md")
	if strings.Count(user, "%s") != 1 || strings.Contains(user, "{{USER_SKILL_DESCRIPTION}}") {
		t.Fatalf("user prompt must contain exactly one %%s and no legacy placeholder: %q", user)
	}
	assertPromptExamplesCompile(t, system)
	assertPromptExamplesCompile(t, user)
}

func assertPromptExamplesCompile(t *testing.T, prompt string) {
	t.Helper()
	for index, match := range promptJSONFence.FindAllStringSubmatch(prompt, -1) {
		definition, err := Parse([]byte(match[1]))
		if err != nil {
			t.Fatalf("prompt JSON example %d does not parse: %v", index, err)
		}
		if _, diagnostics := Compile(definition, DefaultCompileEnvironment()); diagnosticsHaveErrors(diagnostics) {
			t.Fatalf("prompt JSON example %d does not compile: %#v", index, diagnostics)
		}
	}
}

func mustReadPrompt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
