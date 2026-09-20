package cfggen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const groupedMeta = `package: cfg
groups:
  names: [c, s]
  target: [s]
beans:
  - name: DropItem
    fields:
      - { name: item_id, type: int32 }
      - { name: icon,    type: string, group: c }
tables:
  - name: monster
    key: id
    fields:
      - { name: id,        type: int32 }
      - { name: hp,        type: int32, group: s }
      - { name: model,     type: string, group: c }
      - { name: scene_id,  type: int32, index: true }
      - { name: skin_id,   type: int32, index: true, group: c }
      - { name: rewards,   type: "[]DropItem" }
  - name: ui_layout
    key: id
    group: c
    fields:
      - { name: id,   type: int32 }
      - { name: path, type: string }
globals:
  - name: world
    fields:
      - { name: width,      type: int32 }
      - { name: bgm,        type: string, group: c }
  - name: client_options
    group: [c]
    fields:
      - { name: quality, type: int32 }
`

func runCfggenArgs(t *testing.T, meta string, extra ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cfg")
	var stdout bytes.Buffer
	args := append([]string{"-meta", metaPath, "-out", out}, extra...)
	if err := Run(args, &stdout); err != nil {
		return "", stdout.String(), err
	}
	source, err := os.ReadFile(filepath.Join(out, generatedFileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(source), stdout.String(), nil
}

// squash collapses gofmt column alignment so expectations can be written
// without guessing the padding.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustContainAll(t *testing.T, source string, wants ...string) {
	t.Helper()
	squashed := squash(source)
	for _, want := range wants {
		if !strings.Contains(squashed, squash(want)) {
			t.Fatalf("generated code missing %q\n----\n%s", want, source)
		}
	}
}

func mustContainNone(t *testing.T, source string, unwanted ...string) {
	t.Helper()
	squashed := squash(source)
	for _, bad := range unwanted {
		if strings.Contains(squashed, squash(bad)) {
			t.Fatalf("generated code must not contain %q\n----\n%s", bad, source)
		}
	}
}

// The server binding is generated from a meta shared with the client: fields,
// tables and globals marked for other groups are simply not there — no
// struct field, no registration, no accessor — and the run says how many it
// left out.
func TestCfggenExportsOnlyTheTargetGroups(t *testing.T) {
	source, stdout, err := runCfggenArgs(t, groupedMeta)
	if err != nil {
		t.Fatal(err)
	}
	mustContainAll(t, source,
		"ID      int32  `json:\"id\" cfg:\"key\"`",
		"Hp      int32  `json:\"hp\"`",
		"SceneID int32  `json:\"scene_id\" cfg:\"index\"`",
		"Rewards []DropItem `json:\"rewards\"`",
		"ItemID int32 `json:\"item_id\"`",
		"func MonsterBySceneID(",
		"Width int32 `json:\"width\"`",
		"func WorldFrom(",
	)
	mustContainNone(t, source,
		"Model", "SkinID", "MonsterBySkinID", "Icon", "Bgm",
		"UiLayout", "ui_layout", "ClientOptions", "client_options",
	)
	if !strings.Contains(stdout, "export groups [s]: omitted 2 entries and 4 fields") {
		t.Fatalf("run did not report what it left out:\n%s", stdout)
	}
}

// -groups overrides the meta's target: the same meta produces the client
// binding, and a field with no group is in both.
func TestCfggenGroupsFlagSelectsAnotherTarget(t *testing.T) {
	source, _, err := runCfggenArgs(t, groupedMeta, "-groups", "c")
	if err != nil {
		t.Fatal(err)
	}
	mustContainAll(t, source,
		"Model   string `json:\"model\"`",
		"func MonsterBySkinID(",
		"Icon   string `json:\"icon\"`",
		"type UiLayoutCfg struct {",
		"func ClientOptionsFrom(",
		"SceneID int32  `json:\"scene_id\" cfg:\"index\"`",
	)
	mustContainNone(t, source, "Hp int32")

	both, _, err := runCfggenArgs(t, groupedMeta, "-groups", "c,s")
	if err != nil {
		t.Fatal(err)
	}
	mustContainAll(t, both, "Hp int32", "Model string", "type UiLayoutCfg struct {")
}

// A meta without groups generates exactly what it did before: nothing is
// omitted and nothing is reported.
func TestCfggenWithoutGroupsExportsEverything(t *testing.T) {
	_, stdout, err := runCfggenArgs(t, demoMeta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "omitted") {
		t.Fatalf("a meta without groups reported omissions:\n%s", stdout)
	}
}

// Every way a group declaration can be wrong is refused for the reason it
// names, before any code is written.
func TestCfggenRefusesEachInvalidGroupUse(t *testing.T) {
	cases := map[string][2]string{
		"undeclared field group": {
			"groups: {names: [s], target: [s]}\ntables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, group: client }\n",
			`table t field x: group "client" is not declared`,
		},
		"undeclared table group": {
			"groups: {names: [s]}\ntables:\n  - name: t\n    key: id\n    group: c\n    fields:\n      - { name: id, type: int32 }\n",
			`table t: group "c" is not declared`,
		},
		"undeclared target": {
			"groups: {names: [s], target: [x]}\ntables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
			`groups.target: group "x" is not declared`,
		},
		"group declared twice": {
			"groups: {names: [s, s]}\n",
			`group "s" declared twice`,
		},
		"key field excluded": {
			"groups: {names: [c, s], target: [s]}\ntables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32, group: c }\n      - { name: x, type: int32 }\n",
			"table t: key field id cannot be excluded from target groups [s]",
		},
		"ref to an excluded table": {
			"groups: {names: [c, s], target: [s]}\ntables:\n  - name: skin\n    key: id\n    group: c\n    fields:\n      - { name: id, type: int32 }\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: skin_id, type: int32, ref: skin }\n",
			`table t field skin_id: ref target "skin" is excluded from target groups [s]`,
		},
		"global with every field excluded": {
			"groups: {names: [c, s], target: [s]}\nglobals:\n  - name: world\n    fields:\n      - { name: bgm, type: string, group: c }\n",
			"global world: every field is excluded from target groups [s]",
		},
		"group is not a name": {
			"groups: {names: [s]}\ntables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32, group: {a: b} }\n",
			"group must be a name or a list of names",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCfggenArgs(t, "package: cfg\n"+testCase[0])
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase[1]) {
				t.Fatalf("error %q does not contain %q", err, testCase[1])
			}
		})
	}
	if _, _, err := runCfggenArgs(t, "package: cfg\n"+"groups: {names: [s]}\ntables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n", "-groups", "nope"); err == nil || !strings.Contains(err.Error(), `groups.target: group "nope" is not declared`) {
		t.Fatalf("-groups with an undeclared name = %v", err)
	}
}
