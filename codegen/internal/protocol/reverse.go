package protocol

import (
	"bytes"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const reverseProtoIgnoreMarker = marker.Prefix + "reverse_proto ignore=true"

type reverseProtoDef struct {
	enums    []EnumDef
	messages []reverseMessageDef
}

type reverseMessageDef struct {
	name   string
	fields []reverseFieldDef
}

type reverseFieldDef struct {
	name     string
	typeName string
	number   int
	repeated bool
	mapKey   string
	mapVal   string
	oneof    string
}

func generateReverseGoFromProto(path string, pkg string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	def, err := parseReverseProto(string(raw))
	if err != nil {
		return nil, err
	}
	if pkg == "" {
		pkg = "def"
	}
	return generateReverseGo(def, pkg, filepath.Base(path))
}

func parseReverseProto(text string) (reverseProtoDef, error) {
	lines := strings.Split(text, "\n")
	def := reverseProtoDef{}
	var pendingGoDef bool
	for i := 0; i < len(lines); i++ {
		line := cleanProtoLine(lines[i])
		if line == "" {
			continue
		}
		if marker.HasProvenance(line) {
			pendingGoDef = true
			continue
		}
		switch {
		case strings.HasPrefix(line, "enum "):
			enum, next, err := parseReverseEnum(lines, i)
			if err != nil {
				return reverseProtoDef{}, err
			}
			if !pendingGoDef {
				def.enums = append(def.enums, enum)
			}
			pendingGoDef = false
			i = next
		case strings.HasPrefix(line, "message "):
			msg, next, err := parseReverseMessage(lines, i)
			if err != nil {
				return reverseProtoDef{}, err
			}
			if !pendingGoDef {
				def.messages = append(def.messages, msg)
			}
			pendingGoDef = false
			i = next
		default:
			pendingGoDef = false
		}
	}
	return def, nil
}

func parseReverseEnum(lines []string, start int) (EnumDef, int, error) {
	line := cleanProtoLine(lines[start])
	name, err := reverseBlockName(line, "enum")
	if err != nil {
		return EnumDef{}, start, err
	}
	enum := EnumDef{Name: name, Underlying: "int32"}
	for i := start + 1; i < len(lines); i++ {
		line = cleanProtoLine(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "}") {
			return enum, i, nil
		}
		if strings.HasPrefix(line, "option ") || strings.HasPrefix(line, "reserved ") {
			continue
		}
		valueName, value, ok := parseReverseEnumValue(line)
		if !ok {
			continue
		}
		enum.Values = append(enum.Values, EnumValueDef{
			Name:  reverseEnumValueGoName(enum.Name, valueName),
			Value: value,
		})
	}
	return EnumDef{}, start, fmt.Errorf("enum %s missing closing brace", name)
}

func parseReverseMessage(lines []string, start int) (reverseMessageDef, int, error) {
	line := cleanProtoLine(lines[start])
	name, err := reverseBlockName(line, "message")
	if err != nil {
		return reverseMessageDef{}, start, err
	}
	msg := reverseMessageDef{name: name}
	for i := start + 1; i < len(lines); i++ {
		line = cleanProtoLine(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "}") {
			return msg, i, nil
		}
		if strings.HasPrefix(line, "option ") || strings.HasPrefix(line, "reserved ") {
			continue
		}
		if strings.HasPrefix(line, "oneof ") {
			oneofName, next, fields, err := parseReverseOneof(lines, i)
			if err != nil {
				return reverseMessageDef{}, start, err
			}
			for _, field := range fields {
				field.oneof = oneofName
				msg.fields = append(msg.fields, field)
			}
			i = next
			continue
		}
		field, ok, err := parseReverseField(line)
		if err != nil {
			return reverseMessageDef{}, start, err
		}
		if ok {
			msg.fields = append(msg.fields, field)
		}
	}
	return reverseMessageDef{}, start, fmt.Errorf("message %s missing closing brace", name)
}

func parseReverseOneof(lines []string, start int) (string, int, []reverseFieldDef, error) {
	line := cleanProtoLine(lines[start])
	name, err := reverseBlockName(line, "oneof")
	if err != nil {
		return "", start, nil, err
	}
	var fields []reverseFieldDef
	for i := start + 1; i < len(lines); i++ {
		line = cleanProtoLine(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "}") {
			return name, i, fields, nil
		}
		field, ok, err := parseReverseField(line)
		if err != nil {
			return "", start, nil, err
		}
		if ok {
			fields = append(fields, field)
		}
	}
	return "", start, nil, fmt.Errorf("oneof %s missing closing brace", name)
}

var (
	reverseBlockNameRe = regexp.MustCompile(`^(enum|message|oneof)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{?$`)
	reverseEnumValRe   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(-?[0-9]+)\s*;?$`)
	reverseMapFieldRe  = regexp.MustCompile(`^map\s*<\s*([A-Za-z_][A-Za-z0-9_]*)\s*,\s*([A-Za-z_][A-Za-z0-9_]*)\s*>\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([0-9]+).*$`)
	reverseFieldRe     = regexp.MustCompile(`^(repeated\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([0-9]+).*$`)
)

func reverseBlockName(line string, kind string) (string, error) {
	m := reverseBlockNameRe.FindStringSubmatch(line)
	if m == nil || m[1] != kind {
		return "", fmt.Errorf("invalid %s declaration %q", kind, line)
	}
	return m[2], nil
}

func parseReverseEnumValue(line string) (string, int32, bool) {
	m := reverseEnumValRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	v, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return m[1], int32(v), true
}

func parseReverseField(line string) (reverseFieldDef, bool, error) {
	if m := reverseMapFieldRe.FindStringSubmatch(line); m != nil {
		n, err := strconv.Atoi(m[4])
		if err != nil {
			return reverseFieldDef{}, false, err
		}
		return reverseFieldDef{
			name:   m[3],
			mapKey: m[1],
			mapVal: m[2],
			number: n,
		}, true, nil
	}
	m := reverseFieldRe.FindStringSubmatch(line)
	if m == nil {
		return reverseFieldDef{}, false, nil
	}
	n, err := strconv.Atoi(m[4])
	if err != nil {
		return reverseFieldDef{}, false, err
	}
	return reverseFieldDef{
		name:     m[3],
		typeName: m[2],
		number:   n,
		repeated: strings.TrimSpace(m[1]) != "",
	}, true, nil
}

func cleanProtoLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if idx := strings.Index(line, "//"); idx >= 0 {
		if marker.HasProvenance(line[idx:]) {
			return strings.TrimSpace(line[idx+2:])
		}
		line = strings.TrimSpace(line[:idx])
	}
	return line
}

func generateReverseGo(def reverseProtoDef, pkg string, source string) ([]byte, error) {
	enumNames := make(map[string]struct{}, len(def.enums))
	messageNames := make(map[string]struct{}, len(def.messages))
	for _, enum := range def.enums {
		enumNames[enum.Name] = struct{}{}
	}
	for _, msg := range def.messages {
		messageNames[msg.name] = struct{}{}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tool/protocol reverse. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "%s source=%s\n", reverseProtoIgnoreMarker, source)
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	for _, enum := range def.enums {
		emitEnum(&b, enum)
	}
	for _, msg := range def.messages {
		fmt.Fprintf(&b, "type %s struct {\n", msg.name)
		for _, field := range msg.fields {
			tag := strconv.Itoa(field.number)
			if field.oneof != "" {
				tag += ",oneof=" + field.oneof
			}
			goType := reverseFieldGoType(field, enumNames, messageNames)
			fmt.Fprintf(&b, "\t%s %s `pb:%q`\n", reverseGoFieldName(field.name), goType, tag)
		}
		fmt.Fprintf(&b, "}\n\n")
	}
	return format.Source(b.Bytes())
}

func reverseFieldGoType(field reverseFieldDef, enumNames map[string]struct{}, messageNames map[string]struct{}) string {
	if field.mapKey != "" {
		return "map[" + reverseScalarGoType(field.mapKey) + "]" + reverseProtoTypeGoType(field.mapVal, false, enumNames, messageNames)
	}
	if field.repeated {
		return "[]" + reverseProtoTypeGoType(field.typeName, true, enumNames, messageNames)
	}
	return reverseProtoTypeGoType(field.typeName, false, enumNames, messageNames)
}

func reverseProtoTypeGoType(protoType string, repeated bool, enumNames map[string]struct{}, messageNames map[string]struct{}) string {
	if scalar := reverseScalarGoType(protoType); scalar != "" {
		return scalar
	}
	if _, ok := enumNames[protoType]; ok {
		return protoType
	}
	if _, ok := messageNames[protoType]; ok && !repeated {
		return "*" + protoType
	}
	return protoType
}

func reverseScalarGoType(protoType string) string {
	switch protoType {
	case "double":
		return "float64"
	case "float":
		return "float32"
	case "int32", "sint32", "sfixed32":
		return "int32"
	case "uint32", "fixed32":
		return "uint32"
	case "int64", "sint64", "sfixed64":
		return "int64"
	case "uint64", "fixed64":
		return "uint64"
	case "bool":
		return "bool"
	case "string":
		return "string"
	case "bytes":
		return "[]byte"
	default:
		return ""
	}
}

func reverseGoFieldName(protoName string) string {
	parts := strings.Split(protoName, "_")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		switch part {
		case "id":
			b.WriteString("ID")
		case "url":
			b.WriteString("URL")
		case "ip":
			b.WriteString("IP")
		default:
			b.WriteString(strings.ToUpper(part[:1]))
			if len(part) > 1 {
				b.WriteString(part[1:])
			}
		}
	}
	return b.String()
}

func reverseEnumValueGoName(enumName string, protoValue string) string {
	prefix := strings.ToUpper(toSnake(enumName))
	value := strings.TrimPrefix(protoValue, prefix)
	value = strings.TrimPrefix(value, "_")
	if value == "" {
		value = protoValue
	}
	return enumName + reverseGoFieldName(strings.ToLower(value))
}
