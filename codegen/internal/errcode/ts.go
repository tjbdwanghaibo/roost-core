package errcode

import (
	"bytes"
	"fmt"
	"strings"
)

// GenerateTS 产出客户端错误码表 error_codes.ts（roost.yaml 的 client_export 声明 ts 时写进 ts/）：ErrorCodes 常量对象（键是
// errcode 名的 PascalCase，与 C# 的 csharpIdentifier 同一个转换、同一个重名检查，说明写成 JSDoc），另有按码查名字与说明的
// errorCodeName / errorCodeMessage 两个独立函数（C# 是 ErrorCodes.Name / Message；TS 放成函数，常量对象里就没有方法名可撞）。
//
// 与 C# 的差别：C# 撞上类自身符号（Name、Message、Equals……）的常量加后缀 _；TS 的常量对象没有这些成员，不加。PascalCase 转出的
// 键只含 ASCII 字母数字（数字开头时前面补 _），不会是 __proto__ 这类特殊键。
func GenerateTS(defs []Definition) ([]byte, error) {
	idents := make(map[string]string, len(defs))
	names := make([]string, len(defs))
	for i, def := range defs {
		ident, err := csharpIdentifier(def.Name)
		if err != nil {
			return nil, fmt.Errorf("errcode %d (%s): %w", def.Code, def.File, err)
		}
		if other, exists := idents[ident]; exists {
			return nil, fmt.Errorf("errcode names %q and %q both become TS identifier %s; rename one", other, def.Name, ident)
		}
		idents[ident] = def.Name
		names[i] = ident
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n\n/** 服务端 errcode.Define 的错误码（按码查名字与说明用 errorCodeName / errorCodeMessage）。 */\nexport const ErrorCodes = {\n", CSharpGeneratedHeader)
	for i, def := range defs {
		fmt.Fprintf(&b, "  /** %s */\n  %s: %d,\n", jsDoc(def.Message), names[i], def.Code)
	}
	b.WriteString("} as const;\n\nconst names = new Map<number, string>([\n")
	for _, def := range defs {
		fmt.Fprintf(&b, "  [%d, %s],\n", def.Code, jsString(def.Name))
	}
	b.WriteString("]);\n\nconst messages = new Map<number, string>([\n")
	for _, def := range defs {
		fmt.Fprintf(&b, "  [%d, %s],\n", def.Code, jsString(def.Message))
	}
	b.WriteString("]);\n\n/** errcode.Define 的名字；未知码返回 undefined。 */\nexport function errorCodeName(code: number): string | undefined {\n  return names.get(code);\n}\n\n")
	b.WriteString("/** errcode.Define 的说明；未知码返回 undefined。 */\nexport function errorCodeMessage(code: number): string | undefined {\n  return messages.get(code);\n}\n")
	return b.Bytes(), nil
}

// jsString 写 JS 字符串字面量：反斜杠、引号与控制字符转义，行分隔符 U+2028 / U+2029 也转义（老引擎里它们不能出现在字面量中），其余（含中文）原样。
func jsString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsDoc 让说明能放进一行 /** … */：结束记号 */ 拆开，换行改空格。
func jsDoc(text string) string {
	return strings.NewReplacer("*/", "*\\/", "\r\n", " ", "\n", " ", "\r", " ", " ", " ", " ", " ").Replace(text)
}
