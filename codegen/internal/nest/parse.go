package nest

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// FuncInfo describes a handler function to generate code for.
type FuncInfo struct {
	RawName       string        // original function name
	Name          string        // cleaned name (without _cost suffix)
	Entities      []EntityParam // entity parameters
	Params        []NonEntityParam
	Ret           RetParam
	Returns       []RetParam
	Err           ErrorRetParam
	IsCost        bool
	Sync          bool
	Rollback      string
	Durability    string
	RemoteAccess  []RemoteAccessInfo
	SourceImports []ImportInfo // imports needed for type references
	ReceiverType  string       // injected pointer receiver for method handlers
	InvokeName    string       // package function or receiver method expression
}

type RemoteAccessInfo struct {
	Alias          string
	ParamName      string
	RefExpr        string
	Mode           string
	Consistency    string
	Scope          string
	Tenant         string
	Policy         string
	Type           string
	Required       bool
	AllowStale     bool
	MinVersion     string
	CacheTTLMillis string
	Accessor       string
}

type remoteStructFieldInfo struct {
	FieldName string
	Access    RemoteAccessInfo
}

type EntityParam struct {
	Index               int
	Type                string // e.g. "IPlayerEntity"
	Name                string
	GroupType           string // full type including [] prefix
	IsGroup             bool
	IsSpeEntityCategory bool
	EntityCategory      string // e.g. "entity.EntityCategoryPlayer"
	EntityKind          string // e.g. "entity.EntityKindPlayer"
	Target              string // explicit semantic target from //roost:nest
}

type NonEntityParam struct {
	Index int
	Type  string
	Name  string
}

type RetParam struct {
	Type string
	Have bool
}

type ErrorRetParam struct {
	Have bool
}

// ParseResult holds the result of parsing a file.
type ParseResult struct {
	Funcs   []*FuncInfo
	Pkg     string
	Imports []ImportInfo // imports from source file (for type references)
}

// ImportInfo holds an import path and optional alias.
type ImportInfo struct {
	Alias string // empty if no alias
	Path  string
}

// parseFile parses a Go source file and extracts functions marked with
// //roost:nest.
func parseFile(path string) ([]*FuncInfo, string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	// Quick check: skip files without the marker
	if !containsNestMarker(string(src)) {
		return nil, "", nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, "", err
	}

	pkg := file.Name.Name
	remoteStructFields, packageImports, err := collectPackageRemoteStructFields(path, pkg)
	if err != nil {
		return nil, "", err
	}

	// Collect imports from source file
	var imports []ImportInfo
	for _, imp := range file.Imports {
		imports = append(imports, importInfoFromSpec(imp))
	}
	imports = mergeImports(imports, packageImports)

	var funcs []*FuncInfo
	for _, decl := range file.Decls {
		fnDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		marker := parseFuncMarkers(fnDecl)
		if !marker.HasNest {
			continue
		}

		fi, err := parseFuncDecl(fnDecl, marker.NestOptions)
		if err != nil {
			return nil, pkg, fmt.Errorf("nest: handler %s: %w", fnDecl.Name.Name, err)
		}
		if fi == nil {
			continue
		}
		fi.Rollback = marker.NestOptions["rollback"]
		fi.Durability = marker.NestOptions["durability"]
		if err := validateTransactionOptions(fi.Rollback, fi.Durability); err != nil {
			return nil, pkg, fmt.Errorf("nest: handler %s: %w", fnDecl.Name.Name, err)
		}
		fi.Sync = markerOptionEnabled(marker.NestOptions, "sync")
		attachRemoteStructFieldAccess(fi, remoteStructFields)
		funcs = append(funcs, fi)
	}

	// Attach source imports to FuncInfo for code gen
	if len(funcs) > 0 {
		// Determine which imports are actually used by entity/param types
		usedImports := collectUsedImports(funcs, imports)
		for _, f := range funcs {
			f.SourceImports = usedImports
		}
	}

	return funcs, pkg, nil
}

func validateTransactionOptions(rollback string, durability string) error {
	switch rollback {
	case "", "state", "undo":
	default:
		return fmt.Errorf("unsupported rollback policy %q", rollback)
	}
	switch durability {
	case "", "memory", "async", "strict":
	default:
		return fmt.Errorf("unsupported durability policy %q", durability)
	}
	return nil
}

func collectPackageRemoteStructFields(path string, pkg string) (map[string][]remoteStructFieldInfo, []ImportInfo, error) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	ret := make(map[string][]remoteStructFieldInfo)
	var imports []ImportInfo
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_nest_gen.go") || strings.HasPrefix(name, "gen_") {
			continue
		}
		filePath := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filePath, err)
		}
		if file.Name == nil || file.Name.Name != pkg {
			continue
		}
		for _, imp := range file.Imports {
			imports = append(imports, importInfoFromSpec(imp))
		}
		fields, err := collectRemoteStructFields(file)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filePath, err)
		}
		for typeName, items := range fields {
			for _, item := range items {
				if remoteAliasExists(ret[typeName], item.Access.Alias) {
					return nil, nil, fmt.Errorf("nest: duplicate remote alias %q on %s", item.Access.Alias, typeName)
				}
				ret[typeName] = append(ret[typeName], item)
			}
		}
	}
	return ret, mergeImports(nil, imports), nil
}

func importInfoFromSpec(imp *ast.ImportSpec) ImportInfo {
	info := ImportInfo{
		Path: strings.Trim(imp.Path.Value, `"`),
	}
	if imp.Name != nil {
		info.Alias = imp.Name.Name
	}
	return info
}

func mergeImports(base []ImportInfo, extra []ImportInfo) []ImportInfo {
	seen := make(map[string]bool, len(base)+len(extra))
	ret := make([]ImportInfo, 0, len(base)+len(extra))
	for _, imp := range append(base, extra...) {
		key := imp.Alias + "\x00" + imp.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		ret = append(ret, imp)
	}
	return ret
}

func collectRemoteStructFields(file *ast.File) (map[string][]remoteStructFieldInfo, error) {
	ret := make(map[string][]remoteStructFieldInfo)
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok || structType.Fields == nil {
				continue
			}
			for _, field := range structType.Fields.List {
				if field.Tag == nil || len(field.Names) == 0 {
					continue
				}
				tagValue, ok := remoteTagValue(field.Tag.Value)
				if !ok {
					continue
				}
				fieldType := types.ExprString(field.Type)
				if fieldType != "entity.RemoteViewRef" {
					return nil, fmt.Errorf("nest: remote tag field %s.%s must be entity.RemoteViewRef, got %s", typeSpec.Name.Name, field.Names[0].Name, fieldType)
				}
				for _, name := range field.Names {
					access, err := parseRemoteTag(tagValue)
					if err != nil {
						return nil, fmt.Errorf("nest: remote tag %s.%s: %w", typeSpec.Name.Name, name.Name, err)
					}
					if access.Type == "" {
						return nil, fmt.Errorf("nest: remote tag %s.%s missing snapshot type", typeSpec.Name.Name, name.Name)
					}
					if access.Alias == "" {
						access.Alias = aliasFromRemoteRefField(name.Name)
					}
					if access.Accessor == "" {
						access.Accessor = identifierFromAlias(access.Alias)
					}
					if remoteAliasExists(ret[typeSpec.Name.Name], access.Alias) {
						return nil, fmt.Errorf("nest: duplicate remote alias %q on %s", access.Alias, typeSpec.Name.Name)
					}
					ret[typeSpec.Name.Name] = append(ret[typeSpec.Name.Name], remoteStructFieldInfo{
						FieldName: name.Name,
						Access:    access,
					})
				}
			}
		}
	}
	return ret, nil
}

func remoteAliasExists(fields []remoteStructFieldInfo, alias string) bool {
	for _, field := range fields {
		if field.Access.Alias == alias {
			return true
		}
	}
	return false
}

func remoteTagValue(raw string) (string, bool) {
	tag, err := strconv.Unquote(raw)
	if err != nil {
		return "", false
	}
	value := reflect.StructTag(tag).Get("remote")
	return value, value != ""
}

func attachRemoteStructFieldAccess(fi *FuncInfo, remoteStructFields map[string][]remoteStructFieldInfo) {
	if fi == nil || len(remoteStructFields) == 0 {
		return
	}
	for _, p := range fi.Params {
		fields := remoteStructFields[remoteStructLookupType(p.Type)]
		for _, field := range fields {
			access := field.Access
			access.ParamName = p.Name
			access.RefExpr = p.Name + "." + field.FieldName
			if access.MinVersion == "" {
				access.MinVersion = access.RefExpr + ".Version"
			}
			fi.RemoteAccess = append(fi.RemoteAccess, access)
		}
	}
}

func remoteStructLookupType(typeName string) string {
	typeName = strings.TrimPrefix(typeName, "*")
	if dot := strings.LastIndex(typeName, "."); dot >= 0 {
		return typeName[dot+1:]
	}
	return typeName
}

func parseRemoteTag(raw string) (RemoteAccessInfo, error) {
	info := RemoteAccessInfo{Consistency: "monotonic"}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			switch strings.TrimSpace(k) {
			case "alias":
				info.Alias = strings.TrimSpace(v)
			case "accessor":
				info.Accessor = strings.TrimSpace(v)
			case "ttl_ms":
				info.CacheTTLMillis = strings.TrimSpace(v)
			case "min_version":
				info.MinVersion = strings.TrimSpace(v)
			case "consistency":
				info.Consistency = strings.ToLower(strings.TrimSpace(v))
			case "tenant":
				info.Tenant = strings.TrimSpace(v)
			case "policy":
				info.Policy = strings.TrimSpace(v)
			default:
				return info, fmt.Errorf("unknown remote tag option %q", strings.TrimSpace(k))
			}
			continue
		}
		switch strings.ToLower(part) {
		case "cached", "cache":
			info.Consistency = "cached"
		case "monotonic":
			info.Consistency = "monotonic"
		case "strong", "linearizable":
			info.Consistency = "strong"
		case "write":
			return info, fmt.Errorf("write remote tag is not supported by snapshot access; use nest remote entity dispatch")
		case "required":
			info.Required = true
		case "allow_stale":
			info.AllowStale = true
		default:
			if !strings.Contains(part, ".") {
				return info, fmt.Errorf("unknown remote tag option %q", part)
			}
			if info.Type != "" {
				return info, fmt.Errorf("duplicate remote snapshot type %q and %q", info.Type, part)
			}
			info.Type = part
		}
	}
	switch info.Consistency {
	case "cached", "monotonic", "strong", "linearizable":
	default:
		return info, fmt.Errorf("unsupported remote consistency %q", info.Consistency)
	}
	return info, nil
}

func aliasFromRemoteRefField(fieldName string) string {
	base := strings.TrimSuffix(fieldName, "RemoteViewRef")
	base = strings.TrimSuffix(base, "ViewRef")
	base = strings.TrimSuffix(base, "RemoteRef")
	base = strings.TrimSuffix(base, "Ref")
	if base == "" {
		base = fieldName
	}
	return camelToSnake(base)
}

func camelToSnake(raw string) string {
	var b strings.Builder
	for i, r := range raw {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

type funcMarkerInfo struct {
	HasNest     bool
	NestOptions map[string]string
}

func parseFuncMarkers(fnDecl *ast.FuncDecl) funcMarkerInfo {
	ret := funcMarkerInfo{NestOptions: make(map[string]string)}
	if fnDecl.Doc == nil {
		return ret
	}
	for _, c := range fnDecl.Doc.List {
		text := strings.TrimSpace(c.Text)
		switch {
		case marker.Has(text, "nest") && strings.HasPrefix(strings.TrimSpace(text), "//"):
			ret.HasNest = true
			body, _ := marker.Cut(text, "nest")
			ret.NestOptions = parseMarkerOptions(strings.TrimSpace(body))
		}
	}
	return ret
}

func containsNestMarker(src string) bool {
	return marker.Has(src, "nest")
}

func parseMarkerOptions(raw string) map[string]string {
	ret := make(map[string]string)
	for _, part := range strings.Fields(raw) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			ret[part] = "true"
			continue
		}
		ret[k] = strings.Trim(v, `"`)
	}
	return ret
}

func markerOptionEnabled(options map[string]string, key string) bool {
	value := strings.ToLower(strings.TrimSpace(options[key]))
	return value == "true" || value == "1" || value == "yes" || value == "on"
}

func identifierFromAlias(alias string) string {
	var b strings.Builder
	upperNext := true
	for _, r := range alias {
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			if upperNext && r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upperNext = false
			continue
		}
		upperNext = true
	}
	return b.String()
}

// collectUsedImports finds which source imports are referenced by handler entity/param types.
func collectUsedImports(funcs []*FuncInfo, imports []ImportInfo) []ImportInfo {
	// Collect all type strings
	typeRefs := make(map[string]bool)
	for _, f := range funcs {
		for _, e := range f.Entities {
			if dot := strings.Index(e.Type, "."); dot > 0 {
				typeRefs[e.Type[:dot]] = true
			}
		}
		for _, p := range f.Params {
			if dot := strings.Index(p.Type, "."); dot > 0 {
				typeRefs[p.Type[:dot]] = true
			}
		}
		for _, ret := range f.Returns {
			if dot := strings.Index(ret.Type, "."); dot > 0 {
				typeRefs[ret.Type[:dot]] = true
			}
		}
		for _, access := range f.RemoteAccess {
			if dot := strings.Index(access.Scope, "."); dot > 0 {
				typeRefs[access.Scope[:dot]] = true
			}
			if dot := strings.Index(access.Type, "."); dot > 0 {
				typeRefs[access.Type[:dot]] = true
			}
		}
	}

	var used []ImportInfo
	for _, imp := range imports {
		pkgName := imp.Alias
		if pkgName == "" {
			// Derive package name from path
			parts := strings.Split(imp.Path, "/")
			pkgName = parts[len(parts)-1]
		}
		if typeRefs[pkgName] {
			used = append(used, imp)
		}
	}
	return used
}

func parseFuncDecl(fnDecl *ast.FuncDecl, markerOptions map[string]string) (*FuncInfo, error) {
	fi := &FuncInfo{
		RawName:    fnDecl.Name.Name,
		Name:       fnDecl.Name.Name,
		InvokeName: fnDecl.Name.Name,
	}
	if fnDecl.Recv != nil {
		if len(fnDecl.Recv.List) != 1 {
			return nil, errors.New("method handler must have exactly one receiver")
		}
		receiverType := types.ExprString(fnDecl.Recv.List[0].Type)
		if !strings.HasPrefix(receiverType, "*") {
			return nil, errors.New("method handler receiver must be a pointer")
		}
		fi.ReceiverType = receiverType
		fi.RawName = strings.TrimPrefix(receiverType, "*") + "." + fnDecl.Name.Name
		fi.InvokeName = "receiver." + fnDecl.Name.Name
	}

	// Handle _cost suffix
	if strings.HasSuffix(fi.Name, "_cost") {
		fi.IsCost = true
		fi.Name = strings.TrimSuffix(fi.Name, "_cost")
	}

	fnType := fnDecl.Type

	// Parse parameters
	declaredTargets, err := parseDeclaredTargets(markerOptions)
	if err != nil {
		return nil, err
	}
	explicitTargets := len(declaredTargets) > 0
	var hasEntity bool
	var startNonEntity bool
	paramIndex := 0
	for _, field := range fnType.Params.List {
		typeName := types.ExprString(field.Type)

		// Handle multiple names sharing same type
		if len(field.Names) == 0 {
			continue
		}

		for _, name := range field.Names {
			isExplicitEntity := explicitTargets && paramIndex < len(declaredTargets)
			isLegacyGroup := !explicitTargets && isEntityGroupType(typeName)
			isLegacyEntity := !explicitTargets && isEntityCategory(typeName)
			isEntity := isExplicitEntity || isLegacyEntity || isLegacyGroup
			isGroup := isEntity && strings.HasPrefix(typeName, "[]")
			if isEntity {
				hasEntity = true
			} else {
				startNonEntity = true
			}
			if !isEntity || (startNonEntity && !isExplicitEntity && !isLegacyEntity && !isLegacyGroup) {
				fi.Params = append(fi.Params, NonEntityParam{
					Index: len(fi.Params),
					Type:  typeName,
					Name:  usableParamName(name.Name, len(fi.Params)),
				})
			} else {
				baseType := strings.TrimPrefix(typeName, "[]")
				param := EntityParam{
					Index:     len(fi.Entities),
					Type:      baseType,
					Name:      name.Name,
					GroupType: typeName,
					IsGroup:   isGroup,
				}
				if isExplicitEntity {
					param.Target = declaredTargets[paramIndex]
				}
				fi.Entities = append(fi.Entities, param)
			}
			paramIndex++
		}
	}
	if explicitTargets && len(fi.Entities) != len(declaredTargets) {
		return nil, fmt.Errorf("targets declares %d entity parameters, parsed %d", len(declaredTargets), len(fi.Entities))
	}

	if !hasEntity {
		return nil, nil
	}

	// Parse return values
	seenError := false
	if fnType.Results != nil {
		for fieldIndex, field := range fnType.Results.List {
			typeName := types.ExprString(field.Type)
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for i := 0; i < count; i++ {
				if typeName == "error" {
					if seenError || count != 1 || fieldIndex != len(fnType.Results.List)-1 {
						return nil, errors.New("error must be the single final return value")
					}
					seenError = true
					fi.Err.Have = true
					continue
				}
				if seenError {
					return nil, errors.New("non-error return value cannot follow error")
				}
				fi.Returns = append(fi.Returns, RetParam{Type: typeName, Have: true})
			}
		}
	}
	if len(fi.Returns) == 1 {
		fi.Ret = fi.Returns[0]
	}

	return fi, nil
}

func parseDeclaredTargets(options map[string]string) ([]string, error) {
	if len(options) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(options["targets"])
	if one := strings.TrimSpace(options["target"]); one != "" {
		if raw != "" {
			return nil, errors.New("target and targets cannot be used together")
		}
		raw = one
	}
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ret := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("target names must not be empty")
		}
		ret = append(ret, part)
	}
	return ret, nil
}

// isEntityCategory checks if a type name represents an entity parameter.
// Convention: types containing "Entity" (interface types for entity access).
func isEntityCategory(typeName string) bool {
	// Direct entity interface patterns
	if strings.Contains(typeName, "Entity") {
		return true
	}
	return false
}

// isEntityGroupType checks if a type is a slice of entity category.
func isEntityGroupType(typeName string) bool {
	if strings.HasPrefix(typeName, "[]") {
		return isEntityCategory(strings.TrimPrefix(typeName, "[]"))
	}
	return false
}

// usableParamName is the name the generated code uses for one parameter.
//
// `_` is a legal Go parameter name and a natural one for an argument a handler
// no longer reads — but the generated sender copies parameter names into its
// own signature and then PASSES them, so a blank name becomes
// `nest.NewParams(..., _)`: `cannot use _ as value or type`, in a generated
// file, with nothing pointing back at the handler (U-0250). The wire contract
// is positional, so substituting a name changes nothing a caller can observe.
func usableParamName(name string, index int) string {
	if name == "_" || strings.TrimSpace(name) == "" {
		return fmt.Sprintf("arg%d", index)
	}
	return name
}
