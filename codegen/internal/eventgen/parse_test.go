package eventgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEventDir(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	events, err := parseEventDir(dir)
	if err != nil {
		t.Fatalf("parseEventDir: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	names := make(map[string]bool)
	for _, e := range events {
		names[e.Name] = true
	}

	for _, want := range []string{"EventLogin", "EventLogout", "EventLevelUp"} {
		if !names[want] {
			t.Errorf("missing event: %s", want)
		}
	}
	for _, e := range events {
		if e.Name == "EventLevelUp" && len(e.Fields) != 3 {
			t.Fatalf("expected EventLevelUp to have 3 fields, got %d", len(e.Fields))
		}
	}
}

func TestConstName(t *testing.T) {
	cases := []struct{ name, want string }{
		{"EventPlayerOnLine", "EventTypePlayerOnLine"},
		{"EventLogin", "EventTypeLogin"},
	}
	for _, c := range cases {
		e := EventDef{Name: c.name}
		if got := e.ConstName(); got != c.want {
			t.Errorf("ConstName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGenerateDefs(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	events, err := parseEventDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(t.TempDir(), "event_def_gen.go")
	changed, err := generateDefs(events, "event", outFile, true)
	if err != nil {
		t.Fatalf("generateDefs: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)

	checks := []string{
		"package event",
		`"time"`,
		"type EventLogin struct",
		"PId int64",
		"type EventLevelUp struct",
		"Level int32",
		"At",
		"time.Time",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated defs missing: %q", check)
		}
	}
}

func TestGenerateTypes(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	events, err := parseEventDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(t.TempDir(), "event_type_gen.go")
	changed, err := generateTypes(events, "event", outFile, true)
	if err != nil {
		t.Fatalf("generateTypes: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)

	checks := []string{
		"package event",
		"EventTypeInvalid event.EventType = iota",
		"EventTypeLogin",
		"EventTypeLogout",
		"EventTypeLevelUp",
		"EventTypeMaxCount",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated types missing: %q", check)
		}
	}
}

func TestGenerateTypeImpl(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	events, err := parseEventDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(t.TempDir(), "event_type_impl_gen.go")
	changed, err := generateTypeImpl(events, "event", outFile, true)
	if err != nil {
		t.Fatalf("generateTypeImpl: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)

	checks := []string{
		"func (e *EventLogin) Type() event.EventType { return EventTypeLogin }",
		"func (e *EventLogout) Type() event.EventType { return EventTypeLogout }",
		"func (e *EventLevelUp) Type() event.EventType { return EventTypeLevelUp }",
		"_ event.EventData = (*EventLogin)(nil)",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated type impl missing: %q", check)
		}
	}

	// Idempotent
	changed, err = generateTypeImpl(events, "event", outFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no change on second run")
	}
}
