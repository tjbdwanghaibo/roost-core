package entity

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"sort"
	"strings"
	"text/template"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
)

// Mirror 只读视图（PLAN-REMOTE-POLICY-MIRROR 第 5 步，roost-core docs/feature/MIRROR-STEP-5-2026-10-06.md）。
//
// 只读方不再是一个 Entity：它是一个普通 struct（DTO），声明自己要读 owner 快照里的哪些字段：
//
//	//roost:mirror entityKind=guild.EntityKindGuild coll=guild
//	type GuildSummary struct {
//		Name string `bson:"name"`
//	}
//
// 生成器为它写 <dto>_gen_wire.go：视图身份 <DTO>MirrorSpec、解码函数 Decode<DTO>、reader 构造
// New<DTO>Reader。快照的身份规则与 remote=managed 生成的提交快照逐字相同（Scope =
// entity.RemoteSnapshotScope(coll)、Schema = entity.RemoteSnapshotSchema(kind, scope)、Codec 1 = owner DAO 的
// 持久化 BSON 文档），所以 DTO 字段按 owner DAO 的 bson 键声明即可，未声明的字段被忽略。
// 生成物没有 kind / builder / 解码器注册、DAO、提交或发布能力。

// mirrorMarkerRe matches //roost:mirror <params>.
var mirrorMarkerRe = marker.Regexp("mirror", `(?:\s+(.*))?`)

// MirrorDef is a read-only DTO declared with //roost:mirror.
type MirrorDef struct {
	Name       string // DTO struct name
	EntityKind string // owner's entity kind constant expression
	Collection string // owner's DAO collection (//roost:dao coll=...)
	SourceFile string
	Imports    []ImportDef
}

// remoteMirrorMigration is the diagnostic for the retired `remote=mirror` entity form.
const remoteMirrorMigration = `remote=mirror no longer generates an entity: the old form was metadata only (no subscription, no load, no read-only enforcement) and the generated entity stayed writable. A mirror is a read-only DTO now: declare the fields you read on a plain struct marked //roost:mirror entityKind=<owner kind> coll=<owner DAO collection>, regenerate, and read it through the generated New<DTO>Reader(source) — source is kit/remoteentity.MirrorSource(registry) (RemoteMirrorMod in a read-only service, RemoteEntityMod next to the owner)`

// extractMirrors finds //roost:mirror markers and the structs they annotate.
func extractMirrors(fset *token.FileSet, f *ast.File, filePath string, importMap map[string]ImportDef) ([]MirrorDef, error) {
	type markerInfo struct {
		line     int
		params   map[string]string
		attached bool
	}
	var markers []*markerInfo
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			matches := mirrorMarkerRe.FindStringSubmatch(c.Text)
			if matches == nil {
				continue
			}
			line := fset.Position(c.Pos()).Line
			params, err := parseMirrorMarkerParams(matches[1])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: //roost:mirror %w", filePath, line, err)
			}
			markers = append(markers, &markerInfo{line: line, params: params})
		}
	}
	if len(markers) == 0 {
		return nil, nil
	}
	var mirrors []MirrorDef
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
			structLine := fset.Position(typeSpec.Pos()).Line
			for _, m := range markers {
				if m.line != structLine-1 && m.line != structLine-2 {
					continue
				}
				m.attached = true
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					return nil, fmt.Errorf("%s:%d: //roost:mirror must annotate a struct; %s is not one", filePath, m.line, typeSpec.Name.Name)
				}
				if typeSpec.TypeParams != nil {
					return nil, fmt.Errorf("%s:%d: //roost:mirror DTO %s must not be generic", filePath, m.line, typeSpec.Name.Name)
				}
				if err := validateMirrorFields(typeSpec.Name.Name, structType); err != nil {
					return nil, fmt.Errorf("%s:%d: //roost:mirror %w", filePath, m.line, err)
				}
				def := MirrorDef{Name: typeSpec.Name.Name, EntityKind: m.params["entityKind"], Collection: m.params["coll"], SourceFile: filePath}
				if alias := qualifier(def.EntityKind); alias != "" {
					imp, ok := importMap[alias]
					if !ok {
						return nil, fmt.Errorf("%s:%d: //roost:mirror entityKind=%s: package %q is not imported by this file", filePath, m.line, def.EntityKind, alias)
					}
					if imp.Path != "github.com/tjbdwanghaibo/roost-core/entity" {
						def.Imports = append(def.Imports, imp)
					}
				}
				mirrors = append(mirrors, def)
				break
			}
		}
	}
	for _, m := range markers {
		if !m.attached {
			return nil, fmt.Errorf("%s:%d: //roost:mirror marker is not followed by a struct declaration (the `type X struct` line must be within two lines of the marker)", filePath, m.line)
		}
	}
	return mirrors, nil
}

// parseMirrorMarkerParams parses entityKind= and coll=; both are required and nothing else is accepted.
func parseMirrorMarkerParams(s string) (map[string]string, error) {
	params, err := marker.Mirror.Parse(s)
	if err != nil {
		return nil, err
	}
	kind := params["entityKind"]
	if kind == "" {
		return nil, fmt.Errorf("needs entityKind=<the owner's entity kind constant>")
	}
	if !validKindParam(kind) {
		return nil, fmt.Errorf("entityKind=%q is not a constant expression (e.g. EntityKindGuild or guild.EntityKindGuild)", kind)
	}
	coll := params["coll"]
	if coll == "" {
		return nil, fmt.Errorf("needs coll=<the owner's DAO collection, as in its //roost:dao coll=...>")
	}
	if strings.ContainsAny(coll, "\"'`") {
		return nil, fmt.Errorf("coll=%s: write the collection name without quotes", coll)
	}
	return params, nil
}

// validKindParam accepts an identifier, optionally package qualified.
func validKindParam(v string) bool {
	name := v
	if idx := strings.Index(v, "."); idx >= 0 {
		if !isGoIdent(v[:idx]) {
			return false
		}
		name = v[idx+1:]
	}
	return isGoIdent(name)
}

// validateMirrorFields refuses what would make the DTO more than plain data: embedded fields (an entity base
// carried over from the retired remote=mirror entity) and component / DAO fields.
func validateMirrorFields(name string, st *ast.StructType) error {
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			return fmt.Errorf("DTO %s embeds %s: a mirror DTO is plain data read from the owner's snapshot (no entity base, no embedded types)", name, exprToString(field.Type))
		}
		if field.Tag != nil && (strings.Contains(field.Tag.Value, `comp:"`) || strings.Contains(field.Tag.Value, `dao:"`)) {
			return fmt.Errorf("DTO %s field %s has a comp/dao tag: a mirror DTO has no components or DAOs, declare the snapshot fields with bson tags", name, field.Names[0].Name)
		}
	}
	return nil
}

// generateMirror writes the read-only view file for one DTO. Returns true if the file was written.
func generateMirror(def MirrorDef, pkg, outFile string, force bool) (bool, error) {
	var buf bytes.Buffer
	if err := mirrorTemplate.Execute(&buf, mirrorTemplateData{Package: pkg, Mirror: def, ImportBlock: mirrorImportBlock(def)}); err != nil {
		return false, fmt.Errorf("mirror template exec: %w", err)
	}
	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated mirror source: %w", err)
	}
	if !force {
		if existing, err := os.ReadFile(outFile); err == nil && md5.Sum(existing) == md5.Sum(content) {
			return false, nil
		}
	}
	return true, os.WriteFile(outFile, content, 0o644)
}

type mirrorTemplateData struct {
	Package     string
	Mirror      MirrorDef
	ImportBlock string
}

func mirrorImportBlock(def MirrorDef) string {
	lines := []string{`"fmt"`, "", `"github.com/tjbdwanghaibo/roost-core/entity"`, `"go.mongodb.org/mongo-driver/v2/bson"`}
	extra := make([]string, 0, len(def.Imports))
	for _, imp := range def.Imports {
		if imp.Explicit {
			extra = append(extra, fmt.Sprintf("%s %q", imp.Alias, imp.Path))
		} else {
			extra = append(extra, fmt.Sprintf("%q", imp.Path))
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		lines = append(lines, "")
		lines = append(lines, extra...)
	}
	return "import (\n\t" + strings.Join(lines, "\n\t") + "\n)\n"
}

var mirrorTemplate = template.Must(template.New("mirror").Funcs(template.FuncMap{"quote": quoteString}).Parse(`// Code generated by tool/entity. DO NOT EDIT.
package {{.Package}}

{{.ImportBlock}}
// {{.Mirror.Name}}MirrorSpec is the identity of the read-only view {{.Mirror.Name}}: the snapshots the
// owner of {{.Mirror.EntityKind}} (remote=managed) publishes after each commit of its DAO collection
// {{quote .Mirror.Collection}}. The scope / schema / codec rule is the one the generated owner commit uses.
var {{.Mirror.Name}}MirrorSpec = entity.RemoteMirrorSpec{
	Kind:   {{.Mirror.EntityKind}},
	Scope:  entity.RemoteSnapshotScope({{quote .Mirror.Collection}}),
	Schema: entity.RemoteSnapshotSchema({{.Mirror.EntityKind}}, entity.RemoteSnapshotScope({{quote .Mirror.Collection}})),
	// Codec 1: the payload is the owner DAO's persisted BSON document.
	Codec: 1,
}

// Decode{{.Mirror.Name}} decodes one snapshot payload into the DTO. Fields the DTO does not declare are
// ignored. data is the reader's private copy, so the DTO may keep it.
func Decode{{.Mirror.Name}}(data []byte) ({{.Mirror.Name}}, error) {
	var value {{.Mirror.Name}}
	if err := bson.Unmarshal(data, &value); err != nil {
		return {{.Mirror.Name}}{}, fmt.Errorf("{{.Mirror.Name}}: decode mirror snapshot: %w", err)
	}
	return value, nil
}

// New{{.Mirror.Name}}Reader binds the DTO to a read-only snapshot source: kit/remoteentity.MirrorSource(registry)
// (RemoteMirrorMod in a read-only service, or RemoteEntityMod in the owner's process). The reader registers
// nothing process-wide and has no write, commit or publish capability.
func New{{.Mirror.Name}}Reader(source entity.RemoteSnapshotReadOnly) (*entity.RemoteMirrorReader[{{.Mirror.Name}}], error) {
	return entity.NewRemoteMirrorReader(source, {{.Mirror.Name}}MirrorSpec, Decode{{.Mirror.Name}})
}
`))
