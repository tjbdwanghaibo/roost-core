package cfggen

import (
	"strings"
	"testing"
)

// U-0090 (C2): the per-entry field rules of validateMeta must each refuse for
// the reason they name — bean and table/global fields alike. Fixtures keep
// every other rule satisfied so the rule under test is the only refuser.
func TestCfggenRejectsEachFieldRuleForTheStatedReason(t *testing.T) {
	cases := map[string][2]string{
		"bean duplicate field": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: item, type: int32 }\n      - { name: item, type: int32 }\n",
			"bean Drop: duplicate field item",
		},
		"bean fields collide on Go field": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: item_id, type: int32 }\n      - { name: itemID, type: int32 }\n",
			"bean Drop: fields item_id and itemID map to the same Go field ItemID",
		},
		"bean field with ref": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: monster, type: int32, ref: monster }\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
			"bean Drop field monster: ref/index only apply to table fields",
		},
		"bean field with index": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: weight, type: int32, index: true }\n",
			"bean Drop field weight: ref/index only apply to table fields",
		},
		"ref target not a table": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: drop_id, type: int32, ref: drop }\n",
			`table monster field drop_id: ref target "drop" is not a declared table`,
		},
		"duplicate table name": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
			"duplicate table name monster",
		},
		"duplicate global name": {
			"globals:\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n",
			"duplicate global name world",
		},
		"table without fields": {
			"tables:\n  - name: monster\n    key: id\n",
			"table monster: no fields",
		},
		"global without fields": {
			"globals:\n  - name: world\n",
			"global world: no fields",
		},
		"table duplicate field": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: id, type: int32 }\n",
			"table monster: duplicate field id",
		},
		"table fields collide on Go field": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: scene_id, type: int32 }\n      - { name: sceneID, type: int32 }\n",
			"table monster: fields scene_id and sceneID map to the same Go field SceneID",
		},
		"table file escapes data dir": {
			"tables:\n  - name: monster\n    key: id\n    file: ../monster.json\n    fields:\n      - { name: id, type: int32 }\n",
			`table monster: file "../monster.json" escapes the data directory`,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runCfggen(t, "package: cfg\n"+testCase[0])
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase[1]) {
				t.Fatalf("error %q does not contain %q", err, testCase[1])
			}
		})
	}
}
