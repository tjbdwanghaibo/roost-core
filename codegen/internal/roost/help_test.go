package roost

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpCatalogIsCompleteAndUnique(t *testing.T) {
	if err := validateHelpTopics(); err != nil {
		t.Fatal(err)
	}
	names := helpTopicNames()
	for _, required := range []string{
		"environment", "beginner", "project", "versions", "framework-release", "generate", "add", "mods", "dao", "entity", "nest",
		"access", "transport", "lifecycle", "endpoint", "skill", "protocol", "cfggen", "tablegen", "eventgen", "attribute", "webroute",
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
