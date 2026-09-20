package protocol

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var protocolTagNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func toSnake(s string) string {
	var result strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 && !unicode.IsUpper(rune(s[i-1])) {
				result.WriteByte('_')
			}
			result.WriteRune(unicode.ToLower(r))
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

func parseKV(s string) map[string]string {
	params := make(map[string]string)
	for _, p := range strings.Fields(s) {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = strings.Trim(kv[1], `"`)
		}
	}
	return params
}

func parseUint32(s string) (uint32, bool) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 {
		return 0, false
	}
	return uint32(v), true
}

func parseProtocolTags(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var tags []string
	for _, raw := range strings.Split(s, ",") {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if !protocolTagNameRe.MatchString(tag) {
			return nil, strconv.ErrSyntax
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	return tags, nil
}

func parseProtoTag(tag *ast.BasicLit) (int, bool, bool) {
	if tag == nil {
		return 0, false, false
	}
	raw := strings.Trim(tag.Value, "`")
	re := regexp.MustCompile(`pb:"([^"]*)"`)
	m := re.FindStringSubmatch(raw)
	if m == nil {
		return 0, false, false
	}
	if m[1] == "-" {
		return 0, true, true
	}
	parts := strings.Split(m[1], ",")
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, false, true
	}
	return n, false, true
}

func parseProtoTagOptions(tag *ast.BasicLit) (number int, oneof string, skip bool, hasTag bool) {
	number, skip, hasTag = parseProtoTag(tag)
	if !hasTag || skip || tag == nil {
		return number, "", skip, hasTag
	}
	raw := strings.Trim(tag.Value, "`")
	re := regexp.MustCompile(`pb:"([^"]*)"`)
	m := re.FindStringSubmatch(raw)
	if m == nil {
		return number, "", skip, hasTag
	}
	parts := strings.Split(m[1], ",")
	for _, part := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "oneof" {
			oneof = strings.TrimSpace(kv[1])
		}
	}
	return number, oneof, skip, hasTag
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprString(t.Elt)
		}
		return "[" + exprString(t.Len) + "]" + exprString(t.Elt)
	case *ast.BasicLit:
		return t.Value
	default:
		return "any"
	}
}

func exportedStructName(name string) bool {
	if name == "" {
		return false
	}
	return token.IsExported(name)
}

func lower1(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func exportName(s string) string {
	var b strings.Builder
	capNext := true
	for _, r := range s {
		if r == '_' {
			capNext = true
			continue
		}
		if capNext {
			b.WriteRune(unicode.ToUpper(r))
			capNext = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func oneofInterfaceName(parent, oneof string) string {
	return "is" + parent + "_" + exportName(oneof)
}

func oneofMethodName(parent, oneof string) string {
	return "is" + parent + "_" + exportName(oneof)
}

func oneofWrapperName(parent, field string) string {
	return parent + "_" + field
}

func msgName(def MsgDef) string {
	if def.Name != "" {
		return def.Name
	}
	return def.Req
}

func pushName(def PushDef) string {
	if def.Name != "" {
		return def.Name
	}
	return def.Msg
}

func tagConstName(tag string) string {
	var b strings.Builder
	b.WriteString("Tag")
	capNext := true
	for _, r := range tag {
		if r == '_' {
			capNext = true
			continue
		}
		if capNext {
			b.WriteRune(unicode.ToUpper(r))
			capNext = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
