package servicerpc

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// typeIndex holds the named types declared in the package being scanned, so a
// struct passed whole can be checked field by field.
//
// It exists because the first version of this generator checked only the type
// NAME written in the interface, and that missed the exact mistake it was
// meant to catch: mail's SendRequest carries an ExpiresIn time.Duration, so
// `Send(ctx, req SendRequest)` passed validation while putting a nanosecond
// count on the wire under a name that says nothing about units. Checking one
// level deep is checking the part that is easy to get right.
type typeIndex map[string]*ast.StructType

func buildTypeIndex(files map[string]*ast.File) typeIndex {
	index := typeIndex{}
	for _, file := range files {
		for _, decl := range file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if structType, ok := typeSpec.Type.(*ast.StructType); ok {
					index[typeSpec.Name.Name] = structType
				}
			}
		}
	}
	return index
}

// checkWireSafeDeep refuses a type that cannot cross a bus faithfully,
// following same-package structs into their fields.
//
// It stops at package boundaries, and that limit is real rather than swept
// under a comment: a struct from another package cannot be resolved without
// loading that package, so a foreign type is checked by name only. In practice
// a service's request types are declared in the service's own package, which
// is where the mistakes are.
//
// path is the field path so far, so an error names the exact field rather than
// the method's parameter — "req.ExpiresIn" rather than "req".
func checkWireSafeDeep(index typeIndex, where, method, path, typeName string, depth int) error {
	if depth > 8 {
		// A cycle, or a nesting depth nobody should put on a wire.
		return fmt.Errorf("%s: method %s field %s nests more than 8 levels deep; a payload that "+
			"deep is not a payload anyone will debug", where, method, path)
	}
	base := strings.TrimPrefix(strings.TrimPrefix(typeName, "[]"), "*")
	if reason, bad := wireUnsafe[base]; bad {
		return fmt.Errorf("%s: method %s field %s is %s, which cannot cross a bus: %s",
			where, method, path, typeName, reason)
	}
	if strings.HasPrefix(base, "chan ") || strings.HasPrefix(base, "func(") {
		return fmt.Errorf("%s: method %s field %s is %s, which cannot cross a bus",
			where, method, path, typeName)
	}
	if isUnexportedNamed(base) {
		return fmt.Errorf("%s: method %s field %s is the unexported type %s; a caller in "+
			"another package cannot construct it, so it cannot be part of a cross-process API",
			where, method, path, typeName)
	}
	// A map's value can carry an unsafe type too.
	if strings.HasPrefix(base, "map[") {
		if _, value, ok := cutMap(base); ok {
			return checkWireSafeDeep(index, where, method, path+"[]", value, depth+1)
		}
		return nil
	}
	structType, known := index[base]
	if !known {
		// Declared elsewhere. Checked by name above; its fields are the
		// concern of whichever package declares it.
		return nil
	}

	exported := 0
	for _, field := range structType.Fields.List {
		rendered := renderType(field.Type)
		if len(field.Names) == 0 {
			// An embedded field. Its own exported fields are what marshal, so
			// follow it and count it as contributing.
			exported++
			if err := checkWireSafeDeep(index, where, method, path+"."+rendered, rendered, depth+1); err != nil {
				return err
			}
			continue
		}
		for _, ident := range field.Names {
			if !ident.IsExported() {
				// Silently dropped by every codec. That is the SystemToken
				// class: chat's privilege token keeps its authority in an
				// unexported bool, so putting it on a wire marshals it to {}
				// and the peer receives a token that grants nothing. It
				// happens to fail closed, which is luck rather than design —
				// one exported field added to such a type turns the luck into
				// a hole.
				return fmt.Errorf("%s: method %s field %s.%s is unexported; every codec drops it, "+
					"so whatever it carries does not arrive. If it does not matter, remove it "+
					"from the type; if it does, the type cannot cross a bus",
					where, method, path, ident.Name)
			}
			exported++
			if err := checkWireSafeDeep(index, where, method, path+"."+ident.Name, rendered, depth+1); err != nil {
				return err
			}
		}
	}
	if exported == 0 {
		return fmt.Errorf("%s: method %s field %s is %s, which has no exported fields; it "+
			"marshals to {} and arrives as a zero value",
			where, method, path, typeName)
	}
	return nil
}

// cutMap splits "map[K]V" into K and V.
func cutMap(typeName string) (key, value string, ok bool) {
	if !strings.HasPrefix(typeName, "map[") {
		return "", "", false
	}
	depth := 0
	for index := 3; index < len(typeName); index++ {
		switch typeName[index] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return typeName[4:index], typeName[index+1:], true
			}
		}
	}
	return "", "", false
}
