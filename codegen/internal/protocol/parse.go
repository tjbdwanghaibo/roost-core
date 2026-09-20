package protocol

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	protoMarkerRe    = regexp.MustCompile(`^//[a-z]+:proto\s+(.+)$`)
	protocolMarkerRe = regexp.MustCompile(`^//[a-z]+:protocol\s+(.+)$`)
	msgMarkerRe      = regexp.MustCompile(`^//[a-z]+:msg\s+(.+)$`)
	viewMarkerRe     = regexp.MustCompile(`^//[a-z]+:view\s+(.+)$`)
)

type parsedFile struct {
	path        string
	sourceFile  string
	sourceKind  string
	sourceGroup string
	file        *ast.File
}

func parseDefDir(dir string) (*Definitions, error) {
	fset := token.NewFileSet()
	var files []parsedFile

	defs := &Definitions{
		ModulePath:   "example.com/project",
		ProtoPackage: "roost.protocol",
		GoPackage:    "example.com/project/protocol/pb;pb",
	}
	structMap := make(map[string]StructDef)
	enumTypes := make(map[string]string)
	enumValues := make(map[string][]EnumValueDef)
	msgIDs := make(map[uint32]string)

	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "gen_") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if marker.Has(string(content), "reverse_proto ignore=true") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, content, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		sourceFile, err := filepath.Rel(dir, path)
		if err != nil {
			sourceFile = path
		}
		sourceFile = filepath.ToSlash(sourceFile)
		files = append(files, parsedFile{
			path:        path,
			sourceFile:  sourceFile,
			sourceKind:  sourceKindForDefFile(sourceFile),
			sourceGroup: sourceGroupForDefFile(sourceFile),
			file:        f,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].sourceFile < files[j].sourceFile })

	for _, pf := range files {
		f := pf.file
		if err := extractProtoMarkers(f, defs, msgIDs); err != nil {
			return nil, err
		}
		if err := extractInterfaceProtocolMarkers(pf, defs, msgIDs); err != nil {
			return nil, err
		}
		for name, underlying := range extractEnumTypes(f) {
			if old := enumTypes[name]; old != "" {
				return nil, fmt.Errorf("duplicate protocol enum %s with underlying %s and %s", name, old, underlying)
			}
			enumTypes[name] = underlying
		}
		for _, st := range extractStructs(pf) {
			if _, exists := structMap[st.Name]; exists {
				return nil, fmt.Errorf("duplicate protocol struct %s", st.Name)
			}
			structMap[st.Name] = st
		}
	}
	for _, pf := range files {
		if err := extractEnumValues(pf.file, enumTypes, enumValues); err != nil {
			return nil, fmt.Errorf("%s: %w", pf.path, err)
		}
	}

	for name, values := range enumValues {
		defs.Enums = append(defs.Enums, EnumDef{
			Name:       name,
			Underlying: enumTypes[name],
			Values:     values,
		})
	}
	defs.Structs = make([]StructDef, 0, len(structMap))
	for _, st := range structMap {
		defs.Structs = append(defs.Structs, st)
	}
	sort.Slice(defs.Enums, func(i, j int) bool { return defs.Enums[i].Name < defs.Enums[j].Name })
	sort.Slice(defs.Structs, func(i, j int) bool { return defs.Structs[i].Name < defs.Structs[j].Name })
	sort.Slice(defs.Messages, func(i, j int) bool { return defs.Messages[i].ReqID < defs.Messages[j].ReqID })
	sort.Slice(defs.Pushes, func(i, j int) bool { return defs.Pushes[i].MsgID < defs.Pushes[j].MsgID })

	resolveEnumFields(defs)
	if err := validateDefinitions(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func sourceKindForDefFile(sourceFile string) string {
	parts := strings.Split(filepath.ToSlash(sourceFile), "/")
	if len(parts) > 1 {
		switch parts[0] {
		case "protocol", "view", "common":
			return parts[0]
		}
	}
	return "common"
}

func sourceGroupForDefFile(sourceFile string) string {
	name := strings.TrimSuffix(filepath.Base(sourceFile), filepath.Ext(sourceFile))
	if name == "" {
		return "common"
	}
	return name
}

func extractProtoMarkers(f *ast.File, defs *Definitions, msgIDs map[uint32]string) error {
	for _, cg := range f.Comments {
		for _, comment := range cg.List {
			if m := protoMarkerRe.FindStringSubmatch(comment.Text); m != nil {
				params := parseKV(m[1])
				if v := params["package"]; v != "" {
					defs.ProtoPackage = v
				}
				if v := params["go_package"]; v != "" {
					defs.GoPackage = v
				}
			}
		}
	}
	return nil
}

func extractInterfaceProtocolMarkers(pf parsedFile, defs *Definitions, msgIDs map[uint32]string) error {
	f := pf.file
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			iface, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok || iface.Methods == nil {
				continue
			}
			sourceInterface := typeSpec.Name.Name
			group := pf.sourceGroup
			handler := ""
			if params := protocolInterfaceParams(genDecl, typeSpec); params != nil {
				if v := strings.TrimSpace(params["group"]); v != "" {
					group = v
				}
				if v, err := parseProtocolHandler(params, ""); err != nil {
					return fmt.Errorf("roost:protocol interface %s has invalid handler: %w", sourceInterface, err)
				} else {
					handler = v
				}
			}
			for _, field := range iface.Methods.List {
				if len(field.Names) == 0 || field.Doc == nil {
					continue
				}
				fn, ok := field.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				methodName := field.Names[0].Name
				for _, comment := range field.Doc.List {
					m := msgMarkerRe.FindStringSubmatch(comment.Text)
					if m == nil {
						continue
					}
					proto, err := methodMarkerToProtocol(methodName, fn, parseKV(m[1]), handler)
					if err != nil {
						return err
					}
					switch proto.mode {
					case protocolModeReqResp, protocolModePush:
						msg := proto.msg
						msg.Group = group
						msg.SourceFile = pf.sourceFile
						msg.SourceInterface = sourceInterface
						if err := registerMsgID(msgIDs, msg.ReqID, msgName(msg)); err != nil {
							return err
						}
						defs.Messages = append(defs.Messages, msg)
					case protocolModeNotify:
						push := proto.push
						push.Group = group
						push.SourceFile = pf.sourceFile
						push.SourceInterface = sourceInterface
						if err := registerMsgID(msgIDs, push.MsgID, pushName(push)); err != nil {
							return err
						}
						defs.Pushes = append(defs.Pushes, push)
					default:
						return fmt.Errorf("roost:msg method %s has invalid protocol mode", methodName)
					}
				}
			}
		}
	}
	return nil
}

func protocolInterfaceParams(genDecl *ast.GenDecl, typeSpec *ast.TypeSpec) map[string]string {
	if params := firstMarkerParams(typeSpec.Doc, protocolMarkerRe); params != nil {
		return params
	}
	return firstMarkerParams(genDecl.Doc, protocolMarkerRe)
}

type parsedProtocolMode uint8

const (
	protocolModeInvalid parsedProtocolMode = iota
	protocolModeReqResp
	protocolModePush
	protocolModeNotify
)

type parsedProtocol struct {
	mode parsedProtocolMode
	msg  MsgDef
	push PushDef
}

func methodMarkerToProtocol(methodName string, fn *ast.FuncType, params map[string]string, defaultHandler string) (parsedProtocol, error) {
	id, ok := parseUint32(params["id"])
	if !ok {
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s has invalid id", methodName)
	}
	tags, err := parseProtocolTags(params["tags"])
	if err != nil {
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s has invalid tags", methodName)
	}
	handler, err := parseProtocolHandler(params, defaultHandler)
	if err != nil {
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s has invalid handler: %w", methodName, err)
	}
	reqTypes := fieldListTypeNames(fn.Params)
	respTypes := fieldListTypeNames(fn.Results)
	if reqTypes.err != nil {
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s invalid param: %w", methodName, reqTypes.err)
	}
	if respTypes.err != nil {
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s invalid result: %w", methodName, respTypes.err)
	}
	name := params["name"]
	if name == "" {
		name = methodName
	}
	switch {
	case len(reqTypes.names) == 1 && len(respTypes.names) == 1:
		return parsedProtocol{
			mode: protocolModeReqResp,
			msg: MsgDef{
				Name:    name,
				Req:     reqTypes.names[0],
				ReqID:   id,
				Resp:    respTypes.names[0],
				RespID:  id,
				Tags:    tags,
				Handler: handler,
			},
		}, nil
	case len(reqTypes.names) == 1 && len(respTypes.names) == 0:
		return parsedProtocol{
			mode: protocolModePush,
			msg: MsgDef{
				Name:    name,
				Req:     reqTypes.names[0],
				ReqID:   id,
				Tags:    tags,
				Handler: handler,
			},
		}, nil
	case len(reqTypes.names) == 0 && len(respTypes.names) == 1:
		return parsedProtocol{
			mode: protocolModeNotify,
			push: PushDef{
				Name:  name,
				Msg:   respTypes.names[0],
				MsgID: id,
				Tags:  tags,
			},
		}, nil
	default:
		return parsedProtocol{}, fmt.Errorf("roost:msg method %s signature must be req/resp, psh, or ntf", methodName)
	}
}

func parseProtocolHandler(params map[string]string, defaultHandler string) (string, error) {
	handler := params["handler"]
	if handler == "" {
		handler = params["controller"]
	}
	if handler == "" {
		handler = params["domain"]
	}
	if handler == "" {
		handler = defaultHandler
	}
	if strings.TrimSpace(handler) == "" {
		return "", nil
	}
	if !protocolTagNameRe.MatchString(handler) {
		return "", strconv.ErrSyntax
	}
	return handler, nil
}

type fieldListNames struct {
	names []string
	err   error
}

func fieldListTypeNames(list *ast.FieldList) fieldListNames {
	if list == nil || len(list.List) == 0 {
		return fieldListNames{}
	}
	var ret []string
	for _, field := range list.List {
		name, err := protocolTypeName(field.Type)
		if err != nil {
			return fieldListNames{err: err}
		}
		count := 1
		if len(field.Names) > 0 {
			count = len(field.Names)
		}
		for i := 0; i < count; i++ {
			ret = append(ret, name)
		}
	}
	return fieldListNames{names: ret}
}

func protocolTypeName(expr ast.Expr) (string, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		if !exportedStructName(t.Name) {
			return "", fmt.Errorf("protocol type %s must be exported", t.Name)
		}
		return t.Name, nil
	case *ast.StarExpr:
		return protocolTypeName(t.X)
	default:
		return "", fmt.Errorf("unsupported protocol type %s", exprString(expr))
	}
}

func registerMsgID(msgIDs map[uint32]string, id uint32, name string) error {
	if id == 0 {
		return nil
	}
	if old := msgIDs[id]; old != "" {
		return fmt.Errorf("duplicate msg id %d: %s and %s", id, old, name)
	}
	msgIDs[id] = name
	return nil
}

func extractStructs(pf parsedFile) []StructDef {
	f := pf.file
	var ret []StructDef
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || !exportedStructName(typeSpec.Name.Name) {
				continue
			}
			st, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			group := pf.sourceGroup
			if params := firstMarkerParams(typeSpec.Doc, viewMarkerRe); params != nil {
				if v := strings.TrimSpace(params["group"]); v != "" {
					group = v
				}
			} else if params := firstMarkerParams(genDecl.Doc, viewMarkerRe); params != nil {
				if v := strings.TrimSpace(params["group"]); v != "" {
					group = v
				}
			}
			ret = append(ret, StructDef{
				Name:       typeSpec.Name.Name,
				Fields:     extractFields(st),
				Group:      group,
				SourceFile: pf.sourceFile,
				SourceKind: pf.sourceKind,
			})
		}
	}
	return ret
}

func firstMarkerParams(cg *ast.CommentGroup, re *regexp.Regexp) map[string]string {
	if cg == nil {
		return nil
	}
	for _, comment := range cg.List {
		if m := re.FindStringSubmatch(comment.Text); m != nil {
			return parseKV(m[1])
		}
	}
	return nil
}

func extractEnumTypes(f *ast.File) map[string]string {
	ret := make(map[string]string)
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || !exportedStructName(typeSpec.Name.Name) {
				continue
			}
			underlying, ok := enumUnderlying(typeSpec.Type)
			if !ok {
				continue
			}
			ret[typeSpec.Name.Name] = underlying
		}
	}
	return ret
}

func enumUnderlying(expr ast.Expr) (string, bool) {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	switch ident.Name {
	case "int32", "int":
		return ident.Name, true
	default:
		return "", false
	}
}

func extractEnumValues(f *ast.File, enumTypes map[string]string, enumValues map[string][]EnumValueDef) error {
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		var lastType string
		var lastValues []ast.Expr
		for specIdx, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			explicitType := enumConstTypeName(valueSpec.Type, enumTypes)
			typeName := explicitType
			if typeName == "" && len(valueSpec.Values) == 0 {
				typeName = lastType
			}
			if len(valueSpec.Values) > 0 {
				lastValues = valueSpec.Values
			}
			if typeName == "" || len(lastValues) == 0 {
				lastType = ""
				continue
			}
			if explicitType != "" {
				lastType = typeName
			}
			for i, name := range valueSpec.Names {
				if !ast.IsExported(name.Name) {
					continue
				}
				expr := lastValues[len(lastValues)-1]
				if i < len(lastValues) {
					expr = lastValues[i]
				}
				value, err := evalEnumValue(expr, specIdx)
				if err != nil {
					return fmt.Errorf("enum %s value %s: %w", typeName, name.Name, err)
				}
				const maxInt32 = int64(1<<31 - 1)
				if value < 0 || value > maxInt32 {
					return fmt.Errorf("enum %s value %s out of int32 range: %d", typeName, name.Name, value)
				}
				enumValues[typeName] = append(enumValues[typeName], EnumValueDef{
					Name:  name.Name,
					Value: int32(value),
				})
			}
		}
	}
	return nil
}

func enumConstTypeName(expr ast.Expr, enumTypes map[string]string) string {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	if _, ok := enumTypes[ident.Name]; !ok {
		return ""
	}
	return ident.Name
}

func evalEnumValue(expr ast.Expr, iotaValue int) (int64, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return 0, fmt.Errorf("enum value must be integer literal")
		}
		return strconv.ParseInt(e.Value, 0, 64)
	case *ast.Ident:
		if e.Name == "iota" {
			return int64(iotaValue), nil
		}
		return 0, fmt.Errorf("unsupported enum value expression %s", e.Name)
	case *ast.ParenExpr:
		return evalEnumValue(e.X, iotaValue)
	case *ast.UnaryExpr:
		value, err := evalEnumValue(e.X, iotaValue)
		if err != nil {
			return 0, err
		}
		switch e.Op {
		case token.ADD:
			return value, nil
		case token.SUB:
			return -value, nil
		default:
			return 0, fmt.Errorf("unsupported enum unary operator %s", e.Op)
		}
	case *ast.BinaryExpr:
		left, err := evalEnumValue(e.X, iotaValue)
		if err != nil {
			return 0, err
		}
		right, err := evalEnumValue(e.Y, iotaValue)
		if err != nil {
			return 0, err
		}
		switch e.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		default:
			return 0, fmt.Errorf("unsupported enum binary operator %s", e.Op)
		}
	default:
		return 0, fmt.Errorf("unsupported enum value expression %s", exprString(expr))
	}
}

func extractFields(st *ast.StructType) []FieldDef {
	var ret []FieldDef
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		name := field.Names[0].Name
		if !ast.IsExported(name) {
			continue
		}
		number, oneof, skip, hasTag := parseProtoTagOptions(field.Tag)
		if skip {
			continue
		}
		fd := classifyField(field.Type)
		fd.Name = name
		fd.ProtoName = toSnake(name)
		fd.Number = number
		fd.Oneof = oneof
		fd.TypeStr = exprString(field.Type)
		if !hasTag {
			fd.Number = 0
		}
		ret = append(ret, fd)
	}
	return ret
}

func validateDefinitions(defs *Definitions) error {
	if err := validateEnums(defs); err != nil {
		return err
	}
	structs := make(map[string]bool, len(defs.Structs))
	for _, st := range defs.Structs {
		structs[st.Name] = true
		seen := make(map[int]string)
		for _, f := range st.Fields {
			if f.Number <= 0 {
				return fmt.Errorf("%s.%s missing valid pb tag", st.Name, f.Name)
			}
			if old := seen[f.Number]; old != "" {
				return fmt.Errorf("%s field number %d duplicated by %s and %s", st.Name, f.Number, old, f.Name)
			}
			if f.Oneof != "" && !protocolTagNameRe.MatchString(f.Oneof) {
				return fmt.Errorf("%s.%s has invalid oneof name %q", st.Name, f.Name, f.Oneof)
			}
			seen[f.Number] = f.Name
		}
	}
	for _, st := range defs.Structs {
		for _, f := range st.Fields {
			if err := validateField(structs, st.Name+"."+f.Name, f); err != nil {
				return err
			}
		}
	}
	for _, msg := range defs.Messages {
		if !structs[msg.Req] {
			return fmt.Errorf("message req struct %s not found", msg.Req)
		}
		if msg.Resp != "" && !structs[msg.Resp] {
			return fmt.Errorf("message resp struct %s not found", msg.Resp)
		}
		if msg.Resp != "" && msg.RespID != msg.ReqID {
			return fmt.Errorf("message %s req id %d must equal resp id %d", msgName(msg), msg.ReqID, msg.RespID)
		}
	}
	for _, push := range defs.Pushes {
		if !structs[push.Msg] {
			return fmt.Errorf("push struct %s not found", push.Msg)
		}
	}
	return nil
}

func resolveEnumFields(defs *Definitions) {
	enums := make(map[string]EnumDef, len(defs.Enums))
	for _, enum := range defs.Enums {
		enums[enum.Name] = enum
	}
	for si := range defs.Structs {
		for fi := range defs.Structs[si].Fields {
			resolveEnumField(&defs.Structs[si].Fields[fi], enums)
		}
	}
}

func resolveEnumField(f *FieldDef, enums map[string]EnumDef) {
	switch f.Kind {
	case KindMessage:
		enum, ok := enums[f.TypeName]
		if !ok || strings.Contains(f.TypeName, ".") {
			return
		}
		f.Kind = KindEnum
		f.Scalar = ScalarInt32
		f.TypeName = enum.Name
		f.ProtoType = enum.Name
		f.WireType = "varint"
		if f.IsPtr {
			f.GoType = "*" + enum.Name
		} else {
			f.GoType = enum.Name
		}
	case KindRepeated:
		if f.SliceElem == nil {
			return
		}
		resolveEnumField(f.SliceElem, enums)
		f.GoType = "[]" + f.SliceElem.GoType
		f.ProtoType = "repeated " + f.SliceElem.ProtoType
		f.WireType = f.SliceElem.WireType
	case KindMap:
		if f.MapKey != nil {
			resolveEnumField(f.MapKey, enums)
		}
		if f.MapVal != nil {
			resolveEnumField(f.MapVal, enums)
		}
		if f.MapKey != nil && f.MapVal != nil {
			f.GoType = "map[" + f.MapKey.GoType + "]" + f.MapVal.GoType
			f.ProtoType = "map<" + f.MapKey.ProtoType + ", " + f.MapVal.ProtoType + ">"
		}
	}
}

func validateEnums(defs *Definitions) error {
	seenEnums := make(map[string]bool, len(defs.Enums))
	for _, enum := range defs.Enums {
		if seenEnums[enum.Name] {
			return fmt.Errorf("duplicate protocol enum %s", enum.Name)
		}
		seenEnums[enum.Name] = true
		if len(enum.Values) == 0 {
			return fmt.Errorf("enum %s has no values", enum.Name)
		}
		if enum.Values[0].Value != 0 {
			return fmt.Errorf("enum %s first value must be 0", enum.Name)
		}
		seenNames := make(map[string]bool, len(enum.Values))
		seenNumbers := make(map[int32]string, len(enum.Values))
		for _, value := range enum.Values {
			if seenNames[value.Name] {
				return fmt.Errorf("enum %s duplicate value name %s", enum.Name, value.Name)
			}
			seenNames[value.Name] = true
			if old := seenNumbers[value.Value]; old != "" {
				return fmt.Errorf("enum %s duplicate value number %d: %s and %s", enum.Name, value.Value, old, value.Name)
			}
			seenNumbers[value.Value] = value.Name
		}
	}
	return nil
}

func validateField(structs map[string]bool, ctx string, f FieldDef) error {
	if f.IsPtr && (f.Kind == KindScalar || f.Kind == KindEnum || f.Kind == KindBytes) {
		return fmt.Errorf("%s uses pointer scalar/enum/bytes type %s; only pointer messages are supported", ctx, f.TypeStr)
	}
	if f.Kind == KindBytes && strings.HasPrefix(f.TypeStr, "[") && !strings.HasPrefix(f.TypeStr, "[]") {
		return fmt.Errorf("%s uses fixed array bytes type %s; use []byte instead", ctx, f.TypeStr)
	}

	switch f.Kind {
	case KindMessage:
		if strings.Contains(f.TypeName, ".") || !structs[f.TypeName] {
			return fmt.Errorf("%s references unsupported message type %s", ctx, f.TypeName)
		}
	case KindRepeated:
		if f.Oneof != "" {
			return fmt.Errorf("%s uses repeated oneof field %s", ctx, f.TypeStr)
		}
		if f.SliceElem == nil {
			return fmt.Errorf("%s has invalid repeated type", ctx)
		}
		if f.SliceElem.Kind == KindMap {
			return fmt.Errorf("%s uses repeated map, which is not valid proto syntax", ctx)
		}
		return validateField(structs, ctx+"[]", *f.SliceElem)
	case KindMap:
		if f.Oneof != "" {
			return fmt.Errorf("%s uses map oneof field %s", ctx, f.TypeStr)
		}
		if f.MapKey == nil || f.MapVal == nil {
			return fmt.Errorf("%s has invalid map type", ctx)
		}
		if f.MapKey.Kind != KindScalar {
			return fmt.Errorf("%s uses non-scalar map key %s", ctx, f.MapKey.TypeStr)
		}
		switch f.MapKey.Scalar {
		case ScalarString, ScalarBool, ScalarInt32, ScalarInt64, ScalarUint32, ScalarUint64:
		default:
			return fmt.Errorf("%s uses unsupported map key type %s", ctx, f.MapKey.TypeStr)
		}
		if f.MapVal.Kind == KindMap {
			return fmt.Errorf("%s uses nested map value, which is not valid proto syntax", ctx)
		}
		if f.MapVal.Kind == KindRepeated {
			return fmt.Errorf("%s uses repeated map value, which is not valid proto syntax", ctx)
		}
		return validateField(structs, ctx+"{}", *f.MapVal)
	}
	return nil
}
