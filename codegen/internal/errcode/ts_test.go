package errcode

import (
	"strings"
	"testing"
)

// error_codes.ts：键与 C# 同一个 PascalCase 转换；说明进 JSDoc（*/ 拆开、换行改空格）；名字与说明进 Map，字面量转义；
// 两个名字转成同一个标识符时报错；撞上 C# 类自身符号的名字在 TS 里不加后缀。
func TestGenerateTS(t *testing.T) {
	defs := []Definition{
		{Code: 100000, Name: "shop.item_not_found", Message: "商店商品不存在"},
		{Code: 100001, Name: "9lives", Message: "a \"quoted\" \\ */ line\nbreak\u2028"},
		{Code: 100002, Name: "name", Message: "plain"},
		{Code: 100003, Name: "__proto__", Message: "underscores are separators"},
	}
	raw, err := GenerateTS(defs)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		CSharpGeneratedHeader + "\n",
		"export const ErrorCodes = {\n",
		"  /** 商店商品不存在 */\n  ShopItemNotFound: 100000,\n",
		"  /** a \"quoted\" \\ *\\/ line break  */\n  _9lives: 100001,\n",
		"  /** plain */\n  Name: 100002,\n",
		"  /** underscores are separators */\n  Proto: 100003,\n",
		"} as const;\n",
		`  [100000, "shop.item_not_found"],`,
		`  [100001, "a \"quoted\" \\ */ line\nbreak\u2028"],`,
		`  [100003, "__proto__"],`,
		"export function errorCodeName(code: number): string | undefined {\n  return names.get(code);\n}",
		"export function errorCodeMessage(code: number): string | undefined {\n  return messages.get(code);\n}",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("error_codes.ts missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "__proto__:") || strings.Contains(text, "Name_") {
		t.Fatalf("error_codes.ts has a special or C#-suffixed key:\n%s", text)
	}
	if _, err := GenerateTS([]Definition{{Code: 1, Name: "a_b"}, {Code: 2, Name: "a.b"}}); err == nil || !strings.Contains(err.Error(), "both become TS identifier AB") {
		t.Fatalf("identifier collision accepted: %v", err)
	}
	if _, err := GenerateTS([]Definition{{Code: 1, Name: "中文"}}); err == nil {
		t.Fatal("name without ASCII letters accepted")
	}
	empty, err := GenerateTS(nil)
	if err != nil || !strings.Contains(string(empty), "export const ErrorCodes = {\n} as const;") {
		t.Fatalf("empty table: %v\n%s", err, empty)
	}
}
