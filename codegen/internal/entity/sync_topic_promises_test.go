package entity

import "testing"

// U-0239 · C4 · RR-20260918-07：syncTopic 的值要么是显式字符串，要么是限定
// 常量；一个看起来像 Go 常量的裸标识符不能被静默地当成字面量。
//
// 旧行为：syncTopicExpr 只把带点号的导出标识符当常量，其余一律加引号。于是
// `syncTopic=SyncTopicPlayer`（同包常量）生成出 Topic: "SyncTopicPlayer" ——
// 常量的名字，不是它的值。生成成功、编译通过、订阅到一个没人想要的 topic。
func TestSyncTopicRejectsABareIdentifier(t *testing.T) {
	for name, value := range map[string]string{
		"same package constant": "SyncTopicPlayer",
		"exported and short":    "Topic",
	} {
		if err := validateSyncTopicParam(value); err == nil {
			t.Errorf("%s: syncTopic=%s was accepted; it would be written out as the string %q", name, value, value)
		}
	}
}

// The two unambiguous spellings keep working, and a qualified constant is
// still emitted as a reference rather than quoted.
func TestSyncTopicAcceptsLiteralsAndQualifiedConstants(t *testing.T) {
	for name, test := range map[string]struct {
		value string
		want  string
	}{
		"lower case literal": {"player", `"player"`},
		"quoted literal":     {`"Player"`, `"Player"`},
		"qualified constant": {"clientsync.PlayerTopic", "clientsync.PlayerTopic"},
		"empty":              {"", `""`},
	} {
		if err := validateSyncTopicParam(test.value); err != nil {
			t.Errorf("%s: syncTopic=%s was rejected: %v", name, test.value, err)
			continue
		}
		if got := syncTopicExpr(test.value); got != test.want {
			t.Errorf("%s: syncTopicExpr(%q) = %s, want %s", name, test.value, got, test.want)
		}
	}
}
