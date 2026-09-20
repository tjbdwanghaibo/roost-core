package nest

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
)

const generatorVersion = "v1"

type senderMode uint8

const (
	senderModeNone senderMode = iota
	senderModeAsync
	senderModeSync
)

func generate(funcs []*FuncInfo, pkg string, outFile string, force bool, senderOnly bool, registerFunc string) (bool, error) {
	mode := senderModeNone
	if senderOnly {
		mode = senderModeAsync
	}
	return generateWithMode(funcs, pkg, outFile, force, mode, registerFunc)
}

func generateSyncSender(funcs []*FuncInfo, pkg string, outFile string, force bool) (bool, error) {
	return generateWithMode(funcs, pkg, outFile, force, senderModeSync, "")
}

func generateWithMode(funcs []*FuncInfo, pkg string, outFile string, force bool, mode senderMode, registerFunc string) (bool, error) {
	var buf bytes.Buffer
	receiverType, err := commonReceiverType(funcs)
	if err != nil {
		return false, err
	}

	tmpl, err := template.New("nest_gen").Funcs(template.FuncMap{
		"sub":                   func(a, b int) int { return a - b },
		"firstToUpper":          strFirstToUpper,
		"firstToLower":          strFirstToLower,
		"trimHandler":           trimHandlerPrefix,
		"hasGroup":              hasGroup,
		"gt":                    func(a, b int) bool { return a > b },
		"joinEntityIds":         joinEntityIds,
		"extraImports":          extraImports,
		"quote":                 func(s string) string { return fmt.Sprintf("%q", s) },
		"rollbackMeta":          rollbackMeta,
		"remoteParamAccessors":  remoteParamAccessors,
		"remoteKeyName":         remoteKeyName,
		"remoteConsistencyExpr": remoteConsistencyExpr,
		"remoteScopeExpr":       remoteScopeExpr,
		"remoteTTLExpr":         remoteTTLExpr,
	}).Parse(nestTemplate)
	if err != nil {
		return false, fmt.Errorf("template parse: %w", err)
	}

	data := &templateFile{
		Package:         pkg,
		Funcs:           funcs,
		SenderOnly:      mode != senderModeNone,
		AsyncSenderOnly: mode == senderModeAsync,
		SyncSenderOnly:  mode == senderModeSync,
		RegisterFunc:    registerFunc,
		SenderType:      senderTypeName(outFile),
		ReceiverType:    receiverType,
	}
	if err := validateGeneratedTypeImports(funcs, data.SenderOnly, data.SyncSenderOnly); err != nil {
		return false, err
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("template exec: %w", err)
	}

	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated source: %w", err)
	}

	// Check if content changed
	if !force {
		existing, err := os.ReadFile(outFile)
		if err == nil {
			existingHash := fmt.Sprintf("%x", md5.Sum(existing))
			newHash := fmt.Sprintf("%x", md5.Sum(content))
			if existingHash == newHash {
				return false, nil
			}
		}
	}

	if err := os.WriteFile(outFile, content, 0644); err != nil {
		return false, err
	}

	return true, nil
}

type templateFile struct {
	Package         string
	Funcs           []*FuncInfo
	SenderOnly      bool
	AsyncSenderOnly bool
	SyncSenderOnly  bool
	RegisterFunc    string
	SenderType      string
	ReceiverType    string
}

func commonReceiverType(funcs []*FuncInfo) (string, error) {
	var receiver string
	initialized := false
	for _, fn := range funcs {
		if fn == nil {
			continue
		}
		if !initialized {
			receiver = fn.ReceiverType
			initialized = true
			continue
		}
		if fn.ReceiverType != receiver {
			return "", fmt.Errorf("nest: one source file cannot mix receiver %q with %q", receiver, fn.ReceiverType)
		}
	}
	return receiver, nil
}

func senderTypeName(outFile string) string {
	base := strings.TrimSuffix(filepath.Base(outFile), filepath.Ext(outFile))
	base = strings.TrimSuffix(base, "_nest_gen")
	base = strings.TrimPrefix(base, "handler_")
	base = strings.TrimPrefix(base, "handler")
	parts := strings.FieldsFunc(base, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	if b.Len() == 0 {
		b.WriteString("Nest")
	}
	b.WriteString("Sender")
	return b.String()
}

func strFirstToUpper(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func strFirstToLower(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func trimHandlerPrefix(s string) string {
	s = strings.TrimPrefix(s, "handler")
	return strFirstToUpper(s)
}

func hasGroup(entities []EntityParam) bool {
	for _, e := range entities {
		if e.IsGroup {
			return true
		}
	}
	return false
}

func joinEntityIds(entities []EntityParam) string {
	var parts []string
	for _, e := range entities {
		if e.IsGroup {
			parts = append(parts, e.Name)
		} else {
			parts = append(parts, "[]int64{"+e.Name+"}")
		}
	}
	return strings.Join(parts, ", ")
}

func rollbackMeta(f *FuncInfo) string {
	rollback := "nest.RollbackState"
	switch f.Rollback {
	case "":
		if f.Durability == "memory" {
			rollback = "nest.RollbackNone"
		}
	case "state":
		rollback = "nest.RollbackState"
	case "undo":
		rollback = "nest.RollbackUndo"
	}
	durability := "nest.DurabilityAsync"
	switch f.Durability {
	case "memory":
		durability = "nest.DurabilityMemory"
	case "async":
		durability = "nest.DurabilityAsync"
	case "strict":
		durability = "nest.DurabilityStrict"
	}
	if rollback == "nest.RollbackNone" && durability == "nest.DurabilityMemory" {
		return "nest.HandlerMeta{}"
	}
	return "nest.HandlerMeta{Rollback: " + rollback + ", Durability: " + durability + "}"
}

type remoteParamTemplate struct {
	ParamName string
	ParamType string
	Accesses  []RemoteAccessInfo
}

func remoteParamAccessors(funcs []*FuncInfo) []remoteParamTemplate {
	byType := make(map[string]*remoteParamTemplate)
	var order []string
	for _, f := range funcs {
		for _, access := range f.RemoteAccess {
			paramType := remoteParamType(f, access.ParamName)
			if paramType == "" {
				continue
			}
			item := byType[paramType]
			if item == nil {
				item = &remoteParamTemplate{
					ParamName: access.ParamName,
					ParamType: paramType,
				}
				byType[paramType] = item
				order = append(order, paramType)
			}
			if !remoteAccessExists(item.Accesses, access.Alias) {
				item.Accesses = append(item.Accesses, access)
			}
		}
	}
	ret := make([]remoteParamTemplate, 0, len(order))
	for _, typ := range order {
		ret = append(ret, *byType[typ])
	}
	return ret
}

func remoteParamType(f *FuncInfo, paramName string) string {
	for _, p := range f.Params {
		if p.Name == paramName {
			return p.Type
		}
	}
	return ""
}

func remoteAccessExists(accesses []RemoteAccessInfo, alias string) bool {
	for _, access := range accesses {
		if access.Alias == alias {
			return true
		}
	}
	return false
}

func remoteKeyName(paramType string, accessor string) string {
	return "remoteKey" + exportedIdentifier(paramType) + exportedIdentifier(accessor)
}

func exportedIdentifier(raw string) string {
	var b strings.Builder
	upperNext := true
	for _, r := range raw {
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

func remoteConsistencyExpr(consistency string) string {
	switch strings.ToLower(consistency) {
	case "cached", "cache":
		return "entity.RemoteReadCached"
	case "strong", "linearizable":
		return "entity.RemoteReadLinearizable"
	default:
		return "entity.RemoteReadMonotonic"
	}
}

func remoteScopeExpr(access RemoteAccessInfo) string {
	if access.Scope != "" {
		return access.Scope
	}
	if access.Type != "" {
		return "nest.RemoteScopeOf[" + access.Type + "]()"
	}
	return "0"
}

func remoteTTLExpr(access RemoteAccessInfo) string {
	if access.CacheTTLMillis != "" {
		return access.CacheTTLMillis
	}
	if !strings.EqualFold(access.Consistency, "cached") && !strings.EqualFold(access.Consistency, "cache") {
		return "0"
	}
	if access.Type != "" {
		return "nest.RemoteDefaultTTLMillisOf[" + access.Type + "]()"
	}
	return "0"
}

// extraImports collects imports referenced by generated type signatures.
func extraImports(funcs []*FuncInfo, senderOnly bool, syncSenderOnly bool) []ImportInfo {
	typeRefs := generatedTypeRefs(funcs, senderOnly, syncSenderOnly)

	seen := make(map[string]bool)
	resolvedTypeRefs := make(map[string]bool)
	var result []ImportInfo
	for _, f := range funcs {
		for _, imp := range f.SourceImports {
			if seen[imp.Path] {
				continue
			}
			pkgName := imp.Alias
			if pkgName == "" {
				parts := strings.Split(imp.Path, "/")
				pkgName = parts[len(parts)-1]
			}
			if typeRefs[pkgName] {
				resolvedTypeRefs[pkgName] = true
				seen[imp.Path] = true
				result = append(result, imp)
			}
		}
	}
	var unresolved []string
	for pkgName := range typeRefs {
		if !resolvedTypeRefs[pkgName] {
			unresolved = append(unresolved, pkgName)
		}
	}
	sort.Strings(unresolved)
	return result
}

func validateGeneratedTypeImports(funcs []*FuncInfo, senderOnly bool, syncSenderOnly bool) error {
	typeRefs := generatedTypeRefs(funcs, senderOnly, syncSenderOnly)
	if len(typeRefs) == 0 {
		return nil
	}
	resolved := make(map[string]bool)
	for _, f := range funcs {
		for _, imp := range f.SourceImports {
			pkgName := imp.Alias
			if pkgName == "" {
				parts := strings.Split(imp.Path, "/")
				pkgName = parts[len(parts)-1]
			}
			resolved[pkgName] = true
		}
	}
	var unresolved []string
	for pkgName := range typeRefs {
		if !resolved[pkgName] {
			unresolved = append(unresolved, pkgName)
		}
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		return fmt.Errorf("nest: unresolved generated type package alias %q; import it in the handler package", unresolved[0])
	}
	return nil
}

func generatedTypeRefs(funcs []*FuncInfo, senderOnly bool, syncSenderOnly bool) map[string]bool {
	typeRefs := make(map[string]bool)
	track := func(typeName string) {
		for _, pkgName := range packageRefs(typeName) {
			typeRefs[pkgName] = true
		}
	}
	for _, f := range funcs {
		if syncSenderOnly && len(f.Returns) == 0 && !f.Err.Have && !f.Sync {
			continue
		}
		if !senderOnly {
			for _, e := range f.Entities {
				track(e.Type)
			}
		}
		for _, p := range f.Params {
			track(p.Type)
		}
		// Only the sync sender writes a return type's name (its Sync_* /
		// MultiSync_* signatures declare it). The handler-side file assigns
		// the call's result to an `any`, so collecting the return types'
		// packages there produced an import nothing referenced — a handler
		// answering with a type from a third package made its own package
		// fail to compile with "imported and not used" (U-0227). Existing
		// handlers were unaffected only because their result types came from
		// builtins or from a package an entity parameter already imported.
		if syncSenderOnly {
			for _, ret := range f.Returns {
				track(ret.Type)
			}
		}
		if !senderOnly {
			for _, access := range f.RemoteAccess {
				track(access.Scope)
				track(access.Type)
			}
		}
	}
	return typeRefs
}

func packageRefs(typeExpr string) []string {
	if typeExpr == "" {
		return nil
	}
	seen := make(map[string]bool)
	var refs []string
	for _, token := range strings.FieldsFunc(typeExpr, func(r rune) bool {
		return !(r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) {
		dot := strings.Index(token, ".")
		if dot <= 0 {
			continue
		}
		pkgName := token[:dot]
		if !isGoIdentifier(pkgName) || seen[pkgName] {
			continue
		}
		seen[pkgName] = true
		refs = append(refs, pkgName)
	}
	sort.Strings(refs)
	return refs
}

func isGoIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for i, r := range raw {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

var nestTemplate = `// Code generated by tool/nest. DO NOT EDIT.
package {{.Package}}

import (
{{- if not .SenderOnly}}
	"github.com/tjbdwanghaibo/roost-core/entity"
	"errors"
	"sync"
	{{- end}}
	{{- if .SenderOnly}}
		"context"
	{{- end}}
		"github.com/tjbdwanghaibo/roost-core/nest"
	{{- if .AsyncSenderOnly}}
		"time"
	{{- end}}
	{{- range extraImports .Funcs .SenderOnly .SyncSenderOnly}}
		{{if .Alias}}{{.Alias}} {{end}}"{{.Path}}"
	{{- end}}
)

var (
	{{- if .AsyncSenderOnly}}
		_ = time.Second
	{{- end}}
	{{- if not .SenderOnly}}
		_ = errors.New
		_ entity.IThreadSafeEntity
{{- end}}
)
var (
{{- range .Funcs}}
	handlerName{{trimHandler .Name}} = nest.NewHandlerName("{{.RawName}}")
{{- end}}
)
{{if not .SenderOnly}}
var {{firstToLower .RegisterFunc}}Once sync.Once
{{end}}
{{if .SenderOnly}}
// {{.SenderType}} is an instance-scoped, strongly typed Nest client. Construct
// it once at the access boundary and inject it instead of using nest.Nest.
type {{.SenderType}} struct {
	client nest.Client
}

func New{{.SenderType}}(client nest.Client) *{{.SenderType}} {
	return &{{.SenderType}}{client: client}
}

func (s *{{.SenderType}}) nestClient() (nest.Client, error) {
	if s == nil || s.client == nil {
		return nil, nest.ErrNestStopped
	}
	return s.client, nil
}
{{end}}
{{range .Funcs}}
{{$func := .}}
{{if not $.SenderOnly}}
func invoke{{trimHandler .Name}}({{if $.ReceiverType}}receiver {{$.ReceiverType}}, {{end}}es []entity.IThreadSafeEntity, params []any, opts ...nest.HandlerOption) (ret any, err error) {
{{- $rawName := .RawName}}
{{- $handlerName := trimHandler .Name}}
{{- if hasGroup .Entities}}
	optParams := &nest.HandlerOptionParam{}
	for _, opt := range opts {
		opt(optParams)
	}
	if !optParams.IsGroup {
		err = errors.New("nest: expected group dispatch")
		return
	}
	if len(optParams.GroupLen) != {{len .Entities}} {
		err = nest.NewEntityCountMismatchError(handlerName{{trimHandler .Name}}.String(), len(optParams.GroupLen), {{len .Entities}})
		return
	}

	checkELen := 0
	for _, l := range optParams.GroupLen {
		checkELen += l
	}
	if len(es) != checkELen {
		err = nest.NewEntityCountMismatchError(handlerName{{trimHandler .Name}}.String(), len(es), checkELen)
		return
	}

	index := 0
{{- range $i, $p := .Entities}}
	gL{{$p.Index}} := optParams.GroupLen[{{$p.Index}}]
{{- if $p.IsGroup}}
	e{{$p.Index}} := make([]{{$p.Type}}, gL{{$p.Index}})
	for i := 0; i < gL{{$p.Index}}; i++ {
		gei, ok := es[i+index].({{$p.Type}})
		if !ok {
			err = nest.ErrEntityTypeMismatch
			return
		}
		e{{$p.Index}}[i] = gei
	}
{{- else}}
	e{{$p.Index}}, ok := es[index].({{$p.Type}})
	if !ok {
		err = nest.ErrEntityTypeMismatch
		return
	}
{{- end}}
	index += gL{{$p.Index}}
{{- end}}
{{- else}}
	if len(es) != {{len .Entities}} {
		err = nest.NewEntityCountMismatchError(handlerName{{trimHandler .Name}}.String(), len(es), {{len .Entities}})
		return
	}
{{- range $i, $p := .Entities}}
	e{{$p.Index}}, ok := es[{{$i}}].({{$p.Type}})
	if !ok {
		err = nest.ErrEntityTypeMismatch
		return
	}
{{- end}}
{{- end}}

{{- if .Params}}
	if len(params) != {{len .Params}} {
		err = nest.NewParamCountMismatchError(handlerName{{trimHandler .Name}}.String(), len(params), {{len .Params}})
		return
	}
{{- range $i, $p := .Params}}
	p{{$p.Index}}, ok := params[{{$i}}].({{$p.Type}})
	if !ok {
		err = nest.NewParamTypeMismatchError(handlerName{{$handlerName}}.String(), {{$i}}, {{quote $p.Type}}, params[{{$i}}])
		return
	}
{{- end}}
{{- end}}

{{- if gt (len .Returns) 1}}
	{{range $i, $_ := .Returns}}{{if $i}}, {{end}}r{{$i}}{{end}}{{if .Err.Have}}, callErr{{end}} := {{.InvokeName}}({{range $i, $p := .Entities}}{{if $i}}, {{end}}e{{$p.Index}}{{end}}{{if .Entities}}{{if .Params}}, {{end}}{{end}}{{range $i, $p := .Params}}{{if $i}}, {{end}}p{{$p.Index}}{{end}})
	{{- if .Err.Have}}
	err = callErr
	{{- end}}
	ret = []any{ {{range $i, $_ := .Returns}}{{if $i}}, {{end}}r{{$i}}{{end}} }
{{- else if and .Ret.Have .Err.Have}}
	ret, err = {{.InvokeName}}({{range $i, $p := .Entities}}{{if $i}}, {{end}}e{{$p.Index}}{{end}}{{if .Entities}}{{if .Params}}, {{end}}{{end}}{{range $i, $p := .Params}}{{if $i}}, {{end}}p{{$p.Index}}{{end}})
{{- else if .Ret.Have}}
	ret = {{.InvokeName}}({{range $i, $p := .Entities}}{{if $i}}, {{end}}e{{$p.Index}}{{end}}{{if .Entities}}{{if .Params}}, {{end}}{{end}}{{range $i, $p := .Params}}{{if $i}}, {{end}}p{{$p.Index}}{{end}})
{{- else if .Err.Have}}
	err = {{.InvokeName}}({{range $i, $p := .Entities}}{{if $i}}, {{end}}e{{$p.Index}}{{end}}{{if .Entities}}{{if .Params}}, {{end}}{{end}}{{range $i, $p := .Params}}{{if $i}}, {{end}}p{{$p.Index}}{{end}})
{{- else}}
	{{.InvokeName}}({{range $i, $p := .Entities}}{{if $i}}, {{end}}e{{$p.Index}}{{end}}{{if .Entities}}{{if .Params}}, {{end}}{{end}}{{range $i, $p := .Params}}{{if $i}}, {{end}}p{{$p.Index}}{{end}})
{{- end}}
	return
}
{{end}}
	{{if $.SenderOnly}}
	{{- /* Single entity: Broadcast, Delay, Send, Sync */}}
	{{- if eq (len .Entities) 1}}{{if not (index .Entities 0).IsGroup}}
	{{if $.AsyncSenderOnly}}
	func (s *{{$.SenderType}}) Delay_{{trimHandler .Name}}(ctx context.Context, delay time.Duration, id int64{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		return client.Dispatch(ctx, handlerName{{trimHandler .Name}}, id, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}), nest.SendOptionWithDelay(delay){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	func (s *{{$.SenderType}}) Send_{{trimHandler .Name}}(ctx context.Context, id int64{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		return client.Dispatch(ctx, handlerName{{trimHandler .Name}}, id, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	func (s *{{$.SenderType}}) Broadcast_{{trimHandler .Name}}(ctx context.Context, ids []int64{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		return client.DispatchBroadcast(ctx, handlerName{{trimHandler .Name}}, ids, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	{{end}}
	{{if and $.SyncSenderOnly (or (gt (len .Returns) 0) .Err.Have .Sync)}}
	func (s *{{$.SenderType}}) Sync_{{trimHandler .Name}}(ctx context.Context, id int64{{range .Params}}, {{.Name}} {{.Type}}{{end}}) ({{if gt (len .Returns) 1}}{{range $i, $r := .Returns}}ret{{$i}} {{$r.Type}}, {{end}}{{else if .Ret.Have}}ret {{.Ret.Type}}, {{end}}err error) {
		client, clientErr := s.nestClient()
		if clientErr != nil { err = clientErr; return }
		{{if gt (len .Returns) 0}}retXXX, errXXX{{else}}_, errXXX{{end}} := client.Request(ctx, handlerName{{trimHandler .Name}}, id, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
		err = errXXX
		if err != nil { return }
		{{- if gt (len .Returns) 1}}
		if retXXX == nil { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple", retXXX); return }
		retValues, ok := retXXX.([]any)
		if !ok || len(retValues) != {{len .Returns}} { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple[{{len .Returns}}]", retXXX); return }
		{{- range $i, $r := .Returns}}
		ret{{$i}}, ok = retValues[{{$i}}].({{$r.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler $func.Name}}.String(), {{quote $r.Type}}, retValues[{{$i}}]); return }
		{{- end}}
		{{- else if .Ret.Have}}
		if retXXX == nil { return }
		var ok bool
		ret, ok = retXXX.({{.Ret.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), {{quote .Ret.Type}}, retXXX) }
		{{- end}}
		return
	}
	{{end}}
{{- end}}{{end}}

	{{- /* Multi entity (no group): MultiDelay, MultiSend, MultiSync */}}
	{{- if and (gt (len .Entities) 1) (not (hasGroup .Entities))}}
	{{if $.AsyncSenderOnly}}
	func (s *{{$.SenderType}}) MultiDelay_{{trimHandler .Name}}(ctx context.Context, delay time.Duration, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}} int64{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		ids := []int64{ {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{end}} }
		return client.DispatchMulti(ctx, handlerName{{trimHandler .Name}}, ids, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}), nest.SendOptionWithDelay(delay){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	func (s *{{$.SenderType}}) MultiSend_{{trimHandler .Name}}(ctx context.Context, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}} int64{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		ids := []int64{ {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{end}} }
		return client.DispatchMulti(ctx, handlerName{{trimHandler .Name}}, ids, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	{{end}}
	{{if and $.SyncSenderOnly (or (gt (len .Returns) 0) .Err.Have .Sync)}}
	func (s *{{$.SenderType}}) MultiSync_{{trimHandler .Name}}(ctx context.Context, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}} int64{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) ({{if gt (len .Returns) 1}}{{range $i, $r := .Returns}}ret{{$i}} {{$r.Type}}, {{end}}{{else if .Ret.Have}}ret {{.Ret.Type}}, {{end}}err error) {
		client, clientErr := s.nestClient()
		if clientErr != nil { err = clientErr; return }
		ids := []int64{ {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{end}} }
		{{if gt (len .Returns) 0}}retXXX, errXXX{{else}}_, errXXX{{end}} := client.RequestMulti(ctx, handlerName{{trimHandler .Name}}, ids, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
		err = errXXX
		if err != nil { return }
		{{- if gt (len .Returns) 1}}
		if retXXX == nil { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple", retXXX); return }
		retValues, ok := retXXX.([]any)
		if !ok || len(retValues) != {{len .Returns}} { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple[{{len .Returns}}]", retXXX); return }
		{{- range $i, $r := .Returns}}
		ret{{$i}}, ok = retValues[{{$i}}].({{$r.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler $func.Name}}.String(), {{quote $r.Type}}, retValues[{{$i}}]); return }
		{{- end}}
		{{- else if .Ret.Have}}
		if retXXX == nil { return }
		var ok bool
		ret, ok = retXXX.({{.Ret.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), {{quote .Ret.Type}}, retXXX) }
		{{- end}}
		return
	}
{{end}}
{{- end}}

	{{- /* Group entity: MultiGroupDelay, MultiGroupSend, MultiGroupSync */}}
	{{- if hasGroup .Entities}}
	{{if $.AsyncSenderOnly}}
	func (s *{{$.SenderType}}) MultiGroupDelay_{{trimHandler .Name}}(ctx context.Context, delay time.Duration, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{if $p.IsGroup}} []int64{{else}} int64{{end}}{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		groupIDs := [][]int64{ {{joinEntityIds .Entities}} }
		return client.DispatchMultiGroup(ctx, handlerName{{trimHandler .Name}}, groupIDs, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}), nest.SendOptionWithDelay(delay){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	func (s *{{$.SenderType}}) MultiGroupSend_{{trimHandler .Name}}(ctx context.Context, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{if $p.IsGroup}} []int64{{else}} int64{{end}}{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) error {
		client, err := s.nestClient()
		if err != nil { return err }
		groupIDs := [][]int64{ {{joinEntityIds .Entities}} }
		return client.DispatchMultiGroup(ctx, handlerName{{trimHandler .Name}}, groupIDs, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
	}

	{{end}}
	{{if and $.SyncSenderOnly (or (gt (len .Returns) 0) .Err.Have .Sync)}}
	func (s *{{$.SenderType}}) MultiGroupSync_{{trimHandler .Name}}(ctx context.Context, {{range $i, $p := .Entities}}{{if $i}}, {{end}}{{$p.Name}}{{if $p.IsGroup}} []int64{{else}} int64{{end}}{{end}}{{range .Params}}, {{.Name}} {{.Type}}{{end}}) ({{if gt (len .Returns) 1}}{{range $i, $r := .Returns}}ret{{$i}} {{$r.Type}}, {{end}}{{else if .Ret.Have}}ret {{.Ret.Type}}, {{end}}err error) {
		client, clientErr := s.nestClient()
		if clientErr != nil { err = clientErr; return }
		groupIDs := [][]int64{ {{joinEntityIds .Entities}} }
		{{if gt (len .Returns) 0}}retXXX, errXXX{{else}}_, errXXX{{end}} := client.RequestMultiGroup(ctx, handlerName{{trimHandler .Name}}, groupIDs, nest.NewParams({{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}}{{end}}){{if .IsCost}}, nest.SendOptionIsCost(){{end}})
		err = errXXX
		if err != nil { return }
		{{- if gt (len .Returns) 1}}
		if retXXX == nil { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple", retXXX); return }
		retValues, ok := retXXX.([]any)
		if !ok || len(retValues) != {{len .Returns}} { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), "tuple[{{len .Returns}}]", retXXX); return }
		{{- range $i, $r := .Returns}}
		ret{{$i}}, ok = retValues[{{$i}}].({{$r.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler $func.Name}}.String(), {{quote $r.Type}}, retValues[{{$i}}]); return }
		{{- end}}
		{{- else if .Ret.Have}}
		if retXXX == nil { return }
		var ok bool
		ret, ok = retXXX.({{.Ret.Type}})
		if !ok { err = nest.NewResultTypeMismatchError(handlerName{{trimHandler .Name}}.String(), {{quote .Ret.Type}}, retXXX) }
		{{- end}}
		return
	}
{{end}}
{{- end}}
{{end}}
{{end}}

{{if not .SenderOnly}}
{{range remoteParamAccessors .Funcs}}
{{- $paramType := .ParamType}}
{{- $paramName := .ParamName}}
{{- range .Accesses}}
var {{remoteKeyName $paramType .Accessor}} = nest.RemoteKey[{{.Type}}]{Alias: {{quote .Alias}}}
{{- end}}

func ({{.ParamName}} {{.ParamType}}) RemoteAccess() []nest.RemoteAccess {
	return []nest.RemoteAccess{
{{- range .Accesses}}
		{
			Alias: {{quote .Alias}},
			Ref: {{.RefExpr}},
			Consistency: {{remoteConsistencyExpr .Consistency}},
			Scope: {{remoteScopeExpr .}},
			Tenant: {{if .Tenant}}{{.Tenant}}{{else}}0{{end}},
			Policy: {{if .Policy}}{{.Policy}}{{else}}0{{end}},
			MinVersion: {{if .MinVersion}}{{.MinVersion}}{{else}}0{{end}},
			{{- if .AllowStale}}
			AllowStale: true,
			{{- end}}
			CacheTTLMillis: {{remoteTTLExpr .}},
			{{- if .Required}}
			Required: true,
			{{- end}}
		},
{{- end}}
	}
}
{{range .Accesses}}
func ({{$paramName}} {{$paramType}}) {{.Accessor}}() ({{.Type}}, bool) {
	return nest.Remote({{remoteKeyName $paramType .Accessor}})
}

func ({{$paramName}} {{$paramType}}) Must{{.Accessor}}() {{.Type}} {
	return nest.MustRemote({{remoteKeyName $paramType .Accessor}})
}
{{end}}
{{end}}

func {{.RegisterFunc}}({{if .ReceiverType}}receiver {{.ReceiverType}}{{end}}) {
	{{if .ReceiverType}}if receiver == nil { panic("nest: nil handler receiver for {{.RegisterFunc}}") }{{end}}
	{{firstToLower .RegisterFunc}}Once.Do(func() {
{{- range .Funcs}}
		{{- if $.ReceiverType}}
		nest.MustRegisterHandlerWithMeta(handlerName{{trimHandler .Name}}, func(es []entity.IThreadSafeEntity, params []any, opts ...nest.HandlerOption) (any, error) { return invoke{{trimHandler .Name}}(receiver, es, params, opts...) }, {{rollbackMeta .}})
		{{- else}}
		nest.MustRegisterHandlerWithMeta(handlerName{{trimHandler .Name}}, invoke{{trimHandler .Name}}, {{rollbackMeta .}})
		{{- end}}
{{- end}}
	})
}
{{end}}
`

// senderGuardTestTemplate is the companion test generated next to a sender
// package: the nil-client guard inside generated code cannot be reached by the
// generator's own tests, so it is pinned where it is compiled (U-0124).
var senderGuardTestTemplate = `// Code generated by tool/nest. DO NOT EDIT.
package {{.Package}}

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
)

// Test{{.SenderType}}RefusesCallsWithoutAClient pins that a sender constructed without a
// Nest client, or a nil sender, refuses with nest.ErrNestStopped instead of
// dereferencing nil on the first call.
func Test{{.SenderType}}RefusesCallsWithoutAClient(t *testing.T) {
	var none *{{.SenderType}}
	if _, err := none.nestClient(); !errors.Is(err, nest.ErrNestStopped) {
		t.Fatalf("nil sender nestClient() = %v, want nest.ErrNestStopped", err)
	}
	if _, err := New{{.SenderType}}(nil).nestClient(); !errors.Is(err, nest.ErrNestStopped) {
		t.Fatalf("sender without client nestClient() = %v, want nest.ErrNestStopped", err)
	}
}
`

// guardTestFileFor names the companion test of a generated sender file.
func guardTestFileFor(outFile string) string {
	return strings.TrimSuffix(outFile, ".go") + "_test.go"
}

// generateSenderGuardTest writes the companion test for the sender package
// written to outFile.
func generateSenderGuardTest(pkg string, outFile string, force bool) (bool, error) {
	tmpl, err := template.New("nest_sender_guard_test").Parse(senderGuardTestTemplate)
	if err != nil {
		return false, fmt.Errorf("sender guard test template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Package, SenderType string }{Package: pkg, SenderType: senderTypeName(outFile)}); err != nil {
		return false, fmt.Errorf("sender guard test template exec: %w", err)
	}
	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated sender guard test: %w", err)
	}
	return genutil.WriteIfChanged(guardTestFileFor(outFile), content, force)
}
