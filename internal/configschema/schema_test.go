package configschema

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type budget struct {
	Timeout     time.Duration `config:"timeout" min:"1ns"`
	MaxAttempts uint32        `config:"max_attempts" min:"1" max:"1000"`
}

type identity struct {
	Sid int32 `config:"sid" min:"1"`
}

type sample struct {
	identity
	Workers  int           `config:"demo.workers" default:"2" min:"1" example:"8" help:"worker 数"`
	Lease    time.Duration `config:"demo.lease" default:"30s" min:"1ns"`
	Mode     string        `config:"demo.mode" default:"core" enum:"core|jetstream"`
	Database string        `config:"demo.database" required:"true" example:"game"`
	Secret   string        `config:"demo.secret" secret:"true"`
	Peers    []string      `config:"demo.peers" example:"[]"`
	Ratio    float64       `config:"demo.ratio" max:"1"`
	Wal      struct {
		Dir string `config:"dir" example:"data/wal"`
	} `config:"demo.wal" help:"WAL 目录"`
	Defaults budget                       `config:"demo.step_defaults,closed"`
	Steps    map[string]map[string]budget `config:"demo.steps" example:"{}"`
}

func (s *sample) ValidateConfig(production bool) error {
	if s.Workers > 100 {
		return errors.New("config: demo.workers too many for demo")
	}
	return nil
}

func source(t *testing.T, flat map[string]any) Source {
	t.Helper()
	nested := map[string]any{}
	for key, value := range flat {
		node := nested
		parts := strings.Split(key, ".")
		for _, part := range parts[:len(parts)-1] {
			next, ok := node[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[part] = next
			}
			node = next
		}
		node[parts[len(parts)-1]] = value
	}
	return NewMapSource(nested)
}

func TestDecodeAppliesDefaultsAndReadsValues(t *testing.T) {
	var got sample
	err := Decode(source(t, map[string]any{
		"sid": 7, "demo.database": "game", "demo.peers": "a, b,", "demo.mode": " JetStream ",
		"demo.wal.dir": "x", "demo.steps.gift.debit.max_attempts": 15, "demo.lease": "2s",
	}), &got, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sid != 7 || got.Workers != 2 || got.Lease != 2*time.Second || got.Mode != "jetstream" || got.Wal.Dir != "x" ||
		strings.Join(got.Peers, "|") != "a|b" || got.Steps["gift"]["debit"].MaxAttempts != 15 {
		t.Fatalf("decoded %+v", got)
	}
}

func TestDecodeReportsEveryBadKeyByName(t *testing.T) {
	var got sample
	err := Decode(source(t, map[string]any{
		"sid": 0, "demo.workers": "8k", "demo.lease": 15, "demo.mode": "nats", "demo.ratio": 1.5,
		"demo.steps.gift.debit.retries": 3, "demo.steps.gift.credit.max_attempts": 0, "demo.step_defaults.timeout_ms": 5,
		"demo.steps.solo": 5,
	}), &got, false)
	for _, want := range []string{
		"sid must be positive", "demo.workers must be a whole number", "demo.lease = 15 needs a unit",
		"demo.mode must be one of core, jetstream", "demo.ratio must be at most 1", "demo.database is required",
		"demo.steps.gift.debit.retries is not a known key", "demo.steps.gift.credit.max_attempts must be positive",
		"demo.step_defaults.timeout_ms is not a known key", "demo.steps.solo must be a map",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Decode error = %v\nwant it to contain %q", err, want)
		}
	}
}

func TestDecodeRunsValidateConfigOnlyAfterDeclarationsPass(t *testing.T) {
	var got sample
	err := Decode(source(t, map[string]any{"sid": 1, "demo.database": "g", "demo.workers": 200}), &got, false)
	if err == nil || !strings.Contains(err.Error(), "too many for demo") {
		t.Fatalf("Decode = %v, want ValidateConfig's error", err)
	}
}

func TestProductionSecrets(t *testing.T) {
	for _, value := range []any{nil, "", "dev-123"} {
		flat := map[string]any{"sid": 1, "demo.database": "g"}
		if value != nil {
			flat["demo.secret"] = value
		}
		var got sample
		if err := Decode(source(t, flat), &got, true); err == nil || !strings.Contains(err.Error(), "production requires non-dev demo.secret") {
			t.Errorf("secret %v: Decode = %v", value, err)
		}
		if err := Decode(source(t, flat), &got, false); err != nil {
			t.Errorf("secret %v outside production: Decode = %v", value, err)
		}
	}
}

func TestDataSchemaChecksLikeTheStruct(t *testing.T) {
	typed := MustOf(sample{})
	data := Schema{Keys: typed.Keys}
	bad := source(t, map[string]any{"demo.lease": 15, "demo.steps.gift.debit.retries": 3, "demo.mode": "x"})
	got := errors.Join(data.Check(bad, false)...)
	for _, want := range []string{"demo.lease = 15 needs a unit", "demo.steps.gift.debit.retries is not a known key", "demo.mode must be one of", "demo.database is required"} {
		if got == nil || !strings.Contains(got.Error(), want) {
			t.Errorf("Check = %v, want %q", got, want)
		}
	}
	if unknown := data.Unknown(source(t, map[string]any{"demo.wokers": 1, "demo.workers": 1, "other.key": 1})); strings.Join(unknown, ",") != "demo.wokers" {
		t.Errorf("Unknown = %v, want [demo.wokers]", unknown)
	}
}

func TestMergeRefusesConflictingDeclarations(t *testing.T) {
	type a struct {
		X int `config:"s.x" default:"1"`
	}
	type b struct {
		X int `config:"s.x" default:"2"`
	}
	if _, err := Merge(MustOf(a{}), MustOf(a{})); err != nil {
		t.Fatalf("identical declarations: %v", err)
	}
	if _, err := Merge(MustOf(a{}), MustOf(b{})); err == nil || !strings.Contains(err.Error(), "s.x") {
		t.Fatalf("conflicting declarations: %v", err)
	}
}

func TestOfRefusesBadDeclarations(t *testing.T) {
	type badDefault struct {
		X time.Duration `config:"s.x" default:"15"`
	}
	type outOfRange struct {
		X int `config:"s.x" default:"0" min:"1"`
	}
	type untagged struct {
		X int
	}
	for _, value := range []any{badDefault{}, outOfRange{}, untagged{}} {
		if _, err := Of(value); err == nil {
			t.Errorf("Of(%T) = nil error", value)
		}
	}
}

func TestStarterYAML(t *testing.T) {
	got := MustOf(sample{}).StarterYAML(nil)
	want := "demo:\n  # worker 数\n  workers: 8\n  database: game\n  peers: []\n  # WAL 目录\n  wal:\n    dir: data/wal\n  steps: {}\n"
	if got != want {
		t.Fatalf("StarterYAML =\n%s\nwant\n%s", got, want)
	}
}

func TestOptionalSecretKeepsProductionValidation(t *testing.T) {
	type optional struct {
		Token string `config:"token" secret:"optional"`
	}
	schema := MustOf(optional{})
	if !schema.Keys[0].Secret || !schema.Keys[0].SecretOptional {
		t.Fatal("optional secret lost redaction declaration")
	}
	for _, value := range []string{"", "secret-value", "dev-value"} {
		var got optional
		err := Decode(source(t, map[string]any{"token": value}), &got, true)
		if (err != nil) != strings.HasPrefix(value, "dev-") {
			t.Fatalf("optional secret %q: %v", value, err)
		}
	}
}
