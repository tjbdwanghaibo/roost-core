package configschema

import (
	"strconv"
	"strings"
)

// StarterYAML 写出声明里标了 example 的键（生成器写进配置文件的 starter 段）：键上方是 help 注释，值是 example。
// vars 替换 example 里的 {name} 占位符（例如 {project}）。没有 starter 键时返回空串。
func (s Schema) StarterYAML(vars map[string]string) string {
	return s.render(func(key Key) (string, bool) {
		if !key.Starter {
			return "", false
		}
		value := key.Example
		for name, replacement := range vars {
			value = strings.ReplaceAll(value, "{"+name+"}", replacement)
		}
		return value, true
	})
}

// ReferenceYAML 写出全部键（`--print-config`）：starter 键写 example，其余写缺省值，键上方是 help 注释。
func (s Schema) ReferenceYAML() string {
	return s.render(func(key Key) (string, bool) {
		if key.Starter && !strings.Contains(key.Example, "{") {
			return key.Example, true
		}
		return key.Default, true
	})
}

type yamlNode struct {
	name     string
	help     string
	key      *Key
	value    string
	children []*yamlNode
}

func (n *yamlNode) child(name string) *yamlNode {
	for _, child := range n.children {
		if child.name == name {
			return child
		}
	}
	child := &yamlNode{name: name}
	n.children = append(n.children, child)
	return child
}

func (s Schema) render(pick func(Key) (string, bool)) string {
	root := &yamlNode{}
	sectionHelp := map[string]string{}
	for _, key := range s.Keys {
		if key.Kind == KindSection {
			if key.Help != "" {
				sectionHelp[key.Name] = key.Help
			}
			continue
		}
		if strings.Contains(key.Name, "*") {
			continue
		}
		value, ok := pick(key)
		if !ok {
			continue
		}
		node := root
		parts := strings.Split(key.Name, ".")
		for i, part := range parts {
			node = node.child(part)
			if help, ok := sectionHelp[strings.Join(parts[:i+1], ".")]; ok && i < len(parts)-1 {
				node.help = help
			}
		}
		item := key
		node.key = &item
		node.value = value
	}
	var b strings.Builder
	for _, child := range root.children {
		writeNode(&b, child, "")
	}
	return b.String()
}

func writeNode(b *strings.Builder, node *yamlNode, indent string) {
	help := node.help
	if node.key != nil {
		help = node.key.Help
	}
	if help != "" {
		for _, line := range strings.Split(strings.TrimRight(help, "\n"), "\n") {
			b.WriteString(indent)
			b.WriteString(strings.TrimRight("# "+line, " "))
			b.WriteByte('\n')
		}
	}
	if node.key != nil {
		b.WriteString(indent + node.name + ": " + yamlScalar(*node.key, node.value) + "\n")
		return
	}
	b.WriteString(indent + node.name + ":\n")
	for _, child := range node.children {
		writeNode(b, child, indent+"  ")
	}
}

// yamlScalar 把声明里的值写成 YAML：字符串在需要时加引号，空串写 ""，列表写成 [a, b]。
func yamlScalar(key Key, value string) string {
	switch key.Kind {
	case KindString:
		return yamlString(value)
	case KindStrings:
		items, _ := parseValue(key, value)
		list, _ := items.([]string)
		quoted := make([]string, 0, len(list))
		for _, item := range list {
			quoted = append(quoted, yamlString(item))
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	case KindMap:
		if strings.TrimSpace(value) == "" {
			return "{}"
		}
		return value
	}
	if value == "" {
		switch key.Kind {
		case KindBool:
			return "false"
		case KindDuration, KindInt, KindFloat:
			return "0"
		}
	}
	return value
}

func yamlString(value string) string {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "#\"'\n") ||
		strings.Contains(value, ": ") || strings.HasSuffix(value, ":") || strings.ContainsAny(value[:1], "{}[]&*!|>%@`,?-") {
		return strconv.Quote(value)
	}
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", "y", "n":
		return strconv.Quote(value)
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return strconv.Quote(value)
	}
	return value
}
