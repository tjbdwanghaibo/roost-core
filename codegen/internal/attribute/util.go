package attribute

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
	"go/ast"
	"go/format"
	"strconv"
	"strings"
	"unicode"
)

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

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
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

func parsePositiveInt(s string, fallback int) (int, error) {
	if s == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid positive int %q", s)
	}
	return v, nil
}

func attrTag(tag *ast.BasicLit) (name string, skip bool) {
	if tag == nil {
		return "", false
	}
	raw := strings.Trim(tag.Value, "`")
	st := reflectStructTag(raw).Get("attr")
	if st == "" {
		return "", false
	}
	parts := strings.Split(st, ",")
	if strings.TrimSpace(parts[0]) == "-" {
		return "", true
	}
	return strings.TrimSpace(parts[0]), false
}

type reflectStructTag string

func (tag reflectStructTag) Get(key string) string {
	if tag == "" {
		return ""
	}
	for tag != "" {
		tag = trimLeadingSpace(tag)
		if tag == "" {
			break
		}
		i := 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := string(tag[:i])
		tag = tag[i+1:]
		qvalue, rest, ok := scanQuoted(tag)
		if !ok {
			break
		}
		tag = rest
		if key == name {
			value, err := strconv.Unquote(qvalue)
			if err != nil {
				return ""
			}
			return value
		}
	}
	return ""
}

func trimLeadingSpace(s reflectStructTag) reflectStructTag {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return s[i:]
}

func scanQuoted(s reflectStructTag) (qvalue string, rest reflectStructTag, ok bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return string(s[:i+1]), s[i+1:], true
		}
	}
	return "", "", false
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
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

func formatGo(content []byte) ([]byte, error) {
	out, err := format.Source(content)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func writeIfChanged(path string, content []byte, force bool) (bool, error) {
	return genutil.WriteIfChanged(path, content, force)
}
