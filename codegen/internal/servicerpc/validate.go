package servicerpc

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
)

// buildService turns one annotated interface into a Service, refusing
// anything the generator cannot express faithfully.
//
// The refusals are the point of this file. A generator that quietly produced
// something for every input would produce transports that serialize wrongly,
// and a payload that a person cannot read in a packet capture — or that a
// non-Go caller cannot construct — is a payload that looks fine until someone
// has to debug it. Each rule below names the offending method and field, so
// the author fixes the interface rather than guessing.
func buildService(facts pkgFacts, pkgName, path, name string, options map[string]string, iface *ast.InterfaceType) (Service, error) {
	where := fmt.Sprintf("%s: interface %s", path, name)

	serviceType := options["service_type"]
	if serviceType == "" {
		return Service{}, fmt.Errorf("%s: //roost:rpc needs service_type=<name>; it is the bus "+
			"service type both the client and the handlers key on, so it cannot be guessed", where)
	}
	capability := options["capability"]
	if capability == "" {
		return Service{}, fmt.Errorf("%s: //roost:rpc needs capability=<name>; it is the registry "+
			"name both Mods publish under, and the two Mods being mutually exclusive depends on "+
			"them agreeing", where)
	}

	service := Service{
		Package: pkgName, Interface: name,
		ServiceType: serviceType, Capability: capability, File: path,
		hasRequestInvalid: facts.hasRequestInvalid,
	}
	seen := map[string]bool{}
	for _, item := range iface.Methods.List {
		if len(item.Names) == 0 {
			// An embedded interface. Refused rather than followed: the
			// embedded methods would appear in the transport without
			// appearing in this file, so a reader of the interface could not
			// tell what crosses the bus.
			return Service{}, fmt.Errorf("%s: embeds an interface; list the methods that cross "+
				"the bus explicitly, so what is exposed is visible where it is declared", where)
		}
		funcType, ok := item.Type.(*ast.FuncType)
		if !ok {
			continue
		}
		method, err := buildMethod(facts.types, where, item, funcType)
		if err != nil {
			return Service{}, err
		}
		if seen[method.Name] {
			return Service{}, fmt.Errorf("%s: method %s is declared twice", where, method.Name)
		}
		seen[method.Name] = true
		service.Methods = append(service.Methods, method)
	}
	if len(service.Methods) == 0 {
		return Service{}, fmt.Errorf("%s: has no methods", where)
	}
	// The generated transport answers an undecodable payload with this
	// package's ErrRequestInvalid, so the package has to have one.
	//
	// Checked here rather than left to the compiler because the compiler's
	// answer — "undefined: ErrRequestInvalid" in a generated file — sends the
	// reader into generated code to find out what wants it. A malformed
	// payload is a category every wire surface needs an answer for, and
	// answering it with a code that means something else misleads a client
	// that switches on the code.
	if !service.hasRequestInvalid {
		return Service{}, fmt.Errorf("%s: this package has no ErrRequestInvalid, which the "+
			"generated transport reports for a payload it cannot decode. Add one to the "+
			"package's error block with a code from its own segment", where)
	}
	// The generated file declares names at package scope, and a package that
	// already declares one of them produces a file that does not compile.
	//
	// This is not hypothetical. account declared `type Server struct` for a
	// row in its game-server list, and generating a transport whose process
	// host is also called Server broke the package — the compiler answered
	// "Server redeclared in this block" and pointed at the generated file,
	// which says where the duplicate is and nothing about why it exists or
	// which of the two to rename. Worse, a name that collides with a
	// FUNCTION or a CONSTANT can shift a whole package's meaning before
	// anything fails to build.
	//
	// Refusing here names the collision, what already holds the name, and the
	// fact that the generated side cannot move.
	for _, emitted := range emittedNames(service) {
		kind, taken := facts.declared[emitted]
		if !taken {
			continue
		}
		return Service{}, fmt.Errorf("%s: the generated transport declares %s at package scope, "+
			"but this package already declares %s as %s. The generated name is fixed — every "+
			"service's transport uses it, and a caller reads %s.%s the same way in every package "+
			"— so rename the existing one",
			where, emitted, emitted, kind, pkgName, emitted)
	}
	// Affinity is read off an argument at call time, so it has to name one.
	//
	// Two forms are accepted: a bare parameter, which must be a string, and
	// `param.Method()`, for the common case where the key is derived from a
	// value rather than passed as one — a queue identified by a struct with a
	// Key method, say. The parameter's existence is checked here; whether the
	// method exists is the compiler's job, and it reports it precisely.
	for index := range service.Methods {
		method := &service.Methods[index]
		if method.Affinity == "" {
			continue
		}
		paramName, call, isCall := strings.Cut(method.Affinity, ".")
		var found *Field
		for i := range method.Params {
			if method.Params[i].Name == paramName {
				found = &method.Params[i]
				break
			}
		}
		if found == nil {
			return Service{}, fmt.Errorf("%s: method %s declares affinity=%s but has no "+
				"parameter %s; the affinity key is read off an argument at call time",
				where, method.Name, method.Affinity, paramName)
		}
		if !isCall {
			if found.Type != "string" {
				return Service{}, fmt.Errorf("%s: method %s declares affinity=%s, but %s is %s "+
					"and a bare affinity key has to be a string — it is carried in the context "+
					"and hashed to pick an instance. Either pass a string key, or derive one "+
					"with affinity=%s.Key() if %s has such a method",
					where, method.Name, method.Affinity, found.Name, found.Type,
					found.Name, found.Type)
			}
			continue
		}
		if !strings.HasSuffix(call, "()") || strings.ContainsAny(call, " (") != strings.HasSuffix(call, "()") {
			return Service{}, fmt.Errorf("%s: method %s declares affinity=%s; the derived form is "+
				"param.Method() with no arguments, because the key is computed at call time and "+
				"nothing else is in scope", where, method.Name, method.Affinity)
		}
	}
	return service, nil
}

// emittedNames lists every package-scope name the generated file declares,
// in a stable order.
//
// It is written out rather than derived from the template, and that duplication
// is deliberate but not free: a name added to the template and not added here
// is a collision this rule will miss. TestEveryEmittedNameIsListed keeps the
// two in step by scanning the generated golden file, which is the only place
// the truth is observable without executing the template.
func emittedNames(service Service) []string {
	names := []string{
		"ServiceType", "Methods",
		"rpcStatus", "statusOf",
		"RegisterHandlers",
		"DefaultCallTimeout", "BusClient", "NewBusClient",
		"capability", "Capability", "OwnerCapabilities",
		"CapabilityName", "LocalCapabilityName",
		"Server", "NewServer",
		"ClientMod", "NewClientMod",
	}
	for _, method := range service.Methods {
		names = append(names,
			"Method"+method.Name,
			"rpc"+method.Name+"Request",
			"rpc"+method.Name+"Response",
		)
	}
	return names
}

func buildMethod(index typeIndex, where string, item *ast.Field, funcType *ast.FuncType) (Method, error) {
	name := item.Names[0].Name
	method := Method{Name: name, Doc: commentLines(item.Doc)}
	if options, ok := interfaceMarker(item.Doc, item.Comment); ok {
		method.Affinity = options["affinity"]
		_, method.Reliable = options["reliable"]
	}

	// --- parameters ---

	params := flatten(funcType.Params)
	if len(params) == 0 {
		return Method{}, fmt.Errorf("%s: method %s takes no context.Context; every method that "+
			"crosses a bus needs one, or the call cannot be cancelled or timed out", where, name)
	}
	if params[0].Type != "context.Context" {
		return Method{}, fmt.Errorf("%s: method %s takes %s as its first parameter, want "+
			"context.Context", where, name, params[0].Type)
	}
	for _, param := range params[1:] {
		if param.Name == "" || param.Name == "_" {
			return Method{}, fmt.Errorf("%s: method %s has an unnamed parameter of type %s; the "+
				"generator names the wire field after the parameter, so an unnamed one has no "+
				"name to use", where, name, param.Type)
		}
		if err := checkWireSafeDeep(index, where, name, param.Name, param.Type, 0); err != nil {
			return Method{}, err
		}
		method.Params = append(method.Params, param)
	}

	// --- results ---

	results := flatten(funcType.Results)
	if len(results) == 0 || results[len(results)-1].Type != "error" {
		return Method{}, fmt.Errorf("%s: method %s does not end in error; a transport has to be "+
			"able to report that the call failed", where, name)
	}
	for _, result := range results[:len(results)-1] {
		if result.Name == "" || result.Name == "_" {
			return Method{}, fmt.Errorf("%s: method %s has an unnamed result of type %s; the "+
				"generator names the response field after the result, so it must be named. "+
				"Write (%s SomeType, err error)", where, name, result.Type,
				strings.ToLower(result.Type[:1])+result.Type[1:])
		}
		if err := checkWireSafeDeep(index, where, name, result.Name, result.Type, 0); err != nil {
			return Method{}, err
		}
		method.Results = append(method.Results, result)
	}
	return method, nil
}

// wireUnsafe lists the types that must not appear in a payload, with the
// reason a reader of the error needs.
//
// These are refusals rather than conversions on purpose. A generator that
// silently turned a time.Duration into nanoseconds would produce a field
// nobody can read in a capture and no non-Go caller can produce; one that
// turned it into seconds would be guessing at a unit. The author knows which
// unit belongs on the wire, so the author names the field.
var wireUnsafe = map[string]string{
	"time.Duration": "a Duration marshals as an integer nanosecond count, which is unreadable " +
		"in a packet capture and unproducible by a non-Go caller. Put the unit in the name: " +
		"expiresInSeconds int64",
	"time.Time": "a Time marshals as an RFC3339 string whose precision and zone depend on how it " +
		"was built. Carry a unix timestamp: createdAtUnix int64",
	"error": "an error cannot be reconstructed from a payload; the transport already carries the " +
		"failure in the response status",
	"context.Context": "a context does not cross a process boundary",
	"any":             "an untyped payload defeats the point of a typed client",
	"interface{}":     "an untyped payload defeats the point of a typed client",
}

func isUnexportedNamed(typeName string) bool {
	if typeName == "" || strings.Contains(typeName, ".") {
		return false
	}
	switch typeName {
	case "bool", "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune",
		"float32", "float64":
		return false
	}
	if strings.HasPrefix(typeName, "map[") {
		return false
	}
	first := typeName[0]
	return first >= 'a' && first <= 'z'
}

// flatten expands "a, b int" into one Field per name and renders each type.
func flatten(list *ast.FieldList) []Field {
	if list == nil {
		return nil
	}
	out := make([]Field, 0, len(list.List))
	for _, field := range list.List {
		rendered := renderType(field.Type)
		if len(field.Names) == 0 {
			out = append(out, Field{Type: rendered})
			continue
		}
		for _, ident := range field.Names {
			out = append(out, Field{Name: ident.Name, Type: rendered})
		}
	}
	return out
}

// renderType writes a type expression back out as source.
//
// Only the forms a cross-process API may use are handled; anything else
// renders as "?" and is then refused by checkWireSafe, so an unsupported type
// fails with a name rather than generating something wrong.
func renderType(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return renderType(typed.X) + "." + typed.Sel.Name
	case *ast.StarExpr:
		return "*" + renderType(typed.X)
	case *ast.ArrayType:
		if typed.Len == nil {
			return "[]" + renderType(typed.Elt)
		}
		return "?"
	case *ast.MapType:
		return "map[" + renderType(typed.Key) + "]" + renderType(typed.Value)
	case *ast.InterfaceType:
		if len(typed.Methods.List) == 0 {
			return "interface{}"
		}
		return "?"
	case *ast.ChanType:
		return "chan " + renderType(typed.Value)
	case *ast.FuncType:
		return "func("
	default:
		return "?"
	}
}

func commentLines(group *ast.CommentGroup) []string {
	if group == nil {
		return nil
	}
	lines := make([]string, 0, len(group.List))
	for _, comment := range group.List {
		text := comment.Text
		if strings.HasPrefix(text, "//") {
			// The rpc marker is an instruction to the generator, not
			// documentation, so it does not travel into the generated code.
			if _, isMarker := marker.Cut(text, markerKind); isMarker {
				continue
			}
		}
		lines = append(lines, text)
	}
	return lines
}
