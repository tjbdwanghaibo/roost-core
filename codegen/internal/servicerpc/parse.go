// Package servicerpc generates the bus transport for a service interface:
// wire types, handler registration, a typed client, and the capability
// wrappers a Mod publishes.
//
// The input is the hand-written interface itself, not a separate declaration
// file, and that is the design decision the whole generator rests on. The
// interface already exists and it already carries judgement — which of a
// service's methods another process may call is a real decision, and it is
// smaller than the service's own API. A second declaration would be a second
// list, and the transport drifting from the interface is exactly the failure
// this replaces: a method added to the interface and forgotten in the handler
// table compiles perfectly and fails only when another process calls it.
//
// Generating from the interface makes that drift IMPOSSIBLE rather than
// detectable, which is the only reason this generator is worth having. It does
// not save much typing.
package servicerpc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
)

// markerKind is the source marker this generator reads: //roost:rpc.
const markerKind = "rpc"

// Service is one annotated interface.
type Service struct {
	// Package is the Go package the interface lives in.
	Package string
	// Interface is the interface's name, e.g. "Mail".
	Interface string
	// ServiceType is the bus service type, from the interface marker.
	ServiceType string
	// Capability is the registry capability name, from the interface marker.
	Capability string
	// Methods are the interface's methods, in declaration order. Order is
	// preserved rather than sorted so the generated method list reads like the
	// interface a person wrote.
	Methods []Method
	// File is where the interface was found, for error messages.
	File string

	// hasRequestInvalid records whether the package declares the one symbol
	// the generated transport needs from it.
	hasRequestInvalid bool
}

// Method is one interface method that crosses the bus.
type Method struct {
	Name string
	// Params are the parameters after ctx.
	Params []Field
	// Results are the results before the trailing error.
	Results []Field
	// Affinity names the parameter whose value routes the call to a
	// particular instance, or "" for any instance.
	//
	// It exists because one service in this repository genuinely needs it:
	// match keeps a whole queue in one key, so its throughput is bounded by
	// contention on that key, and round-robin routing across replicas is what
	// turns that bound into cross-replica contention. Affinity is therefore a
	// per-method transport property, not a default.
	Affinity string
	// Reliable requests at-least-once delivery.
	Reliable bool
	// Doc is the method's own comment, carried into the generated client so
	// the generated code reads like the interface rather than like a table.
	Doc []string
}

// Field is a named parameter or result.
type Field struct {
	Name string
	// Type is the type as written in the interface, e.g. "int64",
	// "SendRequest", "[]byte".
	Type string
}

// ParseDir finds every //roost:rpc interface under dir.
func ParseDir(dir string) ([]Service, error) {
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool {
		name := info.Name()
		return !strings.HasSuffix(name, "_test.go") && !strings.HasSuffix(name, "_gen.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dir, err)
	}
	services := make([]Service, 0, 1)
	for pkgName, pkg := range packages {
		paths := make([]string, 0, len(pkg.Files))
		for path := range pkg.Files {
			paths = append(paths, path)
		}
		// Sorted, so a directory with several files produces the same output
		// every run. A generator whose output depends on map iteration order
		// makes every regeneration a diff.
		sort.Strings(paths)
		// The type index is built from the WHOLE package, so a request type
		// declared in one file and used in an interface in another is still
		// resolvable. Checking only the file the interface is in would miss
		// exactly the split a real package has.
		facts := pkgFacts{
			types:             buildTypeIndex(pkg.Files),
			declared:          declaredNames(pkg.Files),
			hasRequestInvalid: declaresRequestInvalid(pkg.Files),
		}
		marked := make([]Service, 0, 1)
		for _, path := range paths {
			found, err := parseFile(facts, pkgName, path, pkg.Files[path])
			if err != nil {
				return nil, err
			}
			marked = append(marked, found...)
		}
		// One marked interface per package.
		//
		// The generated file declares Server, CapabilityName, BusClient,
		// capability and a dozen more names at PACKAGE scope, and those names
		// are fixed on purpose — a caller reads pkg.Server and
		// pkg.CapabilityName the same way in every service. Two transports in
		// one package would declare each of them twice, and the two generated
		// files would not compile together.
		//
		// The refusal is not a limitation being worked around; it is an
		// earlier fact restated. app.Service is one per process: a process
		// runs exactly one Serve loop. So two marked interfaces are two
		// things that deploy separately, and two things that deploy
		// separately are two packages. The alternative — prefixing the
		// generated names per interface — would make pkg.Server exist in some
		// packages and pkg.FooServer in others, moving the cost onto every
		// reader of every service to save one package split here.
		if len(marked) > 1 {
			return nil, fmt.Errorf("package %s marks two interfaces, %s and %s: the generated "+
				"transport declares Server, CapabilityName, BusClient and more at package scope, "+
				"so two of them cannot coexist. Two services that deploy as separate processes "+
				"belong in separate packages — split one out rather than renaming the generated "+
				"symbols", pkgName, marked[0].Interface, marked[1].Interface)
		}
		services = append(services, marked...)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Interface < services[j].Interface })
	return services, nil
}

func parseFile(facts pkgFacts, pkgName, path string, file *ast.File) ([]Service, error) {
	var services []Service
	for _, decl := range file.Decls {
		generic, ok := decl.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		// The marker may sit on the GenDecl (a lone type declaration) or on
		// the spec inside a grouped one.
		for _, spec := range generic.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			iface, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			options, ok := interfaceMarker(generic.Doc, typeSpec.Doc)
			if !ok {
				continue
			}
			service, err := buildService(facts, pkgName, path, typeSpec.Name.Name, options, iface)
			if err != nil {
				return nil, err
			}
			services = append(services, service)
		}
	}
	return services, nil
}

// interfaceMarker returns the //roost:rpc options on an interface, if any.
func interfaceMarker(groups ...*ast.CommentGroup) (map[string]string, bool) {
	for _, group := range groups {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			rest, ok := marker.Cut(comment.Text, markerKind)
			if !ok {
				continue
			}
			return parseOptions(rest), true
		}
	}
	return nil, false
}

// parseOptions reads "key=value" and bare "flag" tokens.
func parseOptions(text string) map[string]string {
	options := map[string]string{}
	for _, token := range strings.Fields(text) {
		key, value, found := strings.Cut(token, "=")
		if !found {
			options[key] = ""
			continue
		}
		options[key] = value
	}
	return options
}

// pkgFacts is what the generator knows about the package a marked interface
// lives in. It is gathered once per package and threaded into the
// per-interface validation.
//
// It is a struct rather than a widening parameter list, and that is a lesson
// rather than a style preference: the ErrRequestInvalid rule silently did
// nothing on its first attempt because the flag was computed after parseFile
// returned while the check ran inside buildService. Every package-wide fact
// now travels in one value, so a rule cannot read a zero one by accident.
type pkgFacts struct {
	// types are the package's struct declarations, for field-by-field wire
	// safety checks.
	types typeIndex
	// declared maps every package-scope name to a description of what
	// declared it, so a collision with a generated name can be reported
	// precisely.
	declared map[string]string
	// hasRequestInvalid records whether the package declares the one symbol
	// the generated transport needs from it.
	hasRequestInvalid bool
}

// declaredNames maps each package-scope name to what declared it.
//
// It is used to refuse a package that already declares a name the generated
// file emits. Test files are not scanned, because the parser filter excludes
// them — a test-only declaration of a generated name would still reach the
// compiler rather than this rule. That is a real gap and a narrow one: a test
// that declares Server or BusClient at package scope is not a shape this
// repository has.
func declaredNames(files map[string]*ast.File) map[string]string {
	names := map[string]string{}
	note := func(name, kind string) {
		if name == "_" {
			return
		}
		if _, exists := names[name]; !exists {
			names[name] = kind
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				// Methods are named inside their receiver's scope and cannot
				// collide with a package-scope name.
				if typed.Recv == nil {
					note(typed.Name.Name, "a function")
				}
			case *ast.GenDecl:
				kind := ""
				switch typed.Tok {
				case token.TYPE:
					kind = "a type"
				case token.CONST:
					kind = "a constant"
				case token.VAR:
					kind = "a variable"
				default:
					continue
				}
				for _, spec := range typed.Specs {
					switch typedSpec := spec.(type) {
					case *ast.TypeSpec:
						note(typedSpec.Name.Name, kind)
					case *ast.ValueSpec:
						for _, name := range typedSpec.Names {
							note(name.Name, kind)
						}
					}
				}
			}
		}
	}
	return names
}

// declaresRequestInvalid reports whether the package declares
// ErrRequestInvalid, which the generated transport reports for an undecodable
// payload.
//
// A name check rather than a type check: the generator runs on a directory in
// isolation and cannot load the package. Finding the identifier is enough to
// give a useful error early, and the compiler still has the last word on
// whether it is the right kind of thing.
func declaresRequestInvalid(files map[string]*ast.File) bool {
	for _, file := range files {
		for _, decl := range file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.VAR {
				continue
			}
			for _, spec := range generic.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if name.Name == "ErrRequestInvalid" {
						return true
					}
				}
			}
		}
	}
	return false
}
