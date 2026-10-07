package marker

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// Spec is the option vocabulary of one marker kind: what may follow
// "//roost:<Kind>".
//
// Every generator used to split the options itself, and most of them kept
// whatever keys they recognised and dropped the rest. A misspelt key was then
// read as "not written" and replaced by the default: `durabilty=strict`
// generated an async handler, `dbscop=sid` wrote into the global database,
// and nothing was reported (RR-20261006-56). Parse is now the one place
// options are split, and it refuses anything outside the vocabulary.
type Spec struct {
	// Kind is the marker name after the prefix, e.g. "nest".
	Kind string
	// Keys are accepted as key=value.
	Keys []string
	// Flags are accepted as a bare word; Parse records them with value "true".
	// A name may be both a key and a flag (nest's `sync` / `sync=true`).
	Flags []string
	// Renamed maps a retired key to its replacement. The old spelling is
	// refused with a pointer to the new one rather than silently aliased.
	Renamed map[string]string
}

// The marker kinds that take options. Each generator validates its own kinds
// against these; TestEveryMarkerRefusesAMisspeltKey feeds each one a typo
// through the generator that owns it.
var (
	Nest      = Spec{Kind: "nest", Keys: []string{"rollback", "durability", "sync", "target", "targets"}, Flags: []string{"sync"}}
	Dao       = Spec{Kind: "dao", Keys: []string{"coll", "db", "dbscope", "schema"}, Flags: []string{"nocoll"}}
	RedisDao  = Spec{Kind: "redisdao", Keys: []string{"mode", "key", "key_type", "prefix", "version", "ttl", "name"}}
	Attribute = Spec{Kind: "attribute", Keys: []string{"index", "max"}}
	Proto     = Spec{Kind: "proto", Keys: []string{"package", "go_package"}}
	Protocol  = Spec{Kind: "protocol", Keys: []string{"group", "handler", "controller", "domain"}}
	Msg       = Spec{Kind: "msg", Keys: []string{"id", "name", "tags", "handler", "controller", "domain"}}
	View      = Spec{Kind: "view", Keys: []string{"group"}}
	Table     = Spec{Kind: "table", Keys: []string{"name", "file", "json", "key"}}
	Object    = Spec{Kind: "object", Keys: []string{"name", "file", "json"}}
	// RPC covers both places the marker goes: service_type / capability on
	// the interface, affinity / reliable on a method.
	RPC    = Spec{Kind: "rpc", Keys: []string{"service_type", "capability", "affinity"}, Flags: []string{"reliable"}}
	Entity = Spec{
		Kind: "entity",
		Keys: []string{"id", "entityKind", "category", "remote", "noPersist", "lifetime", "sync", "syncNamespace", "syncPacker", "subjectPacker"},
		// Renamed with ARCH-10: the value is the wire namespace every update
		// of the subject carries, and "topic" suggested a bus that does not exist.
		Renamed: map[string]string{"syncTopic": "syncNamespace"},
	}
	Mirror   = Spec{Kind: "mirror", Keys: []string{"entityKind", "coll"}}
	Web      = Spec{Kind: "web", Keys: []string{"method", "path", "body"}}
	Register = Spec{Kind: "register", Keys: []string{"phase", "order"}}
)

// Specs is every marker kind that takes options.
var Specs = []Spec{Nest, Dao, RedisDao, Attribute, Proto, Protocol, Msg, View, Table, Object, RPC, Entity, Mirror, Web, Register}

// known lists the vocabulary for error messages: keys as "key=", flags bare.
func (s Spec) known() string {
	names := make([]string, 0, len(s.Keys)+len(s.Flags))
	for _, key := range s.Keys {
		names = append(names, key+"=")
	}
	for _, flag := range s.Flags {
		names = append(names, flag)
	}
	return strings.Join(names, " ")
}

// Parse splits the options that follow the marker (the text after
// "//roost:<Kind>") into a map. It refuses an unknown or renamed key, a key
// given twice, a bare word that is not a flag and a flag given a value that
// is not also a key. The error names the offending token but not the
// position; callers add file:line (CheckFile does).
func (s Spec) Parse(body string) (map[string]string, error) {
	options := make(map[string]string)
	for _, token := range strings.Fields(body) {
		key, value, hasValue := strings.Cut(token, "=")
		if newKey, renamed := s.Renamed[key]; renamed {
			return nil, fmt.Errorf("marker option %q was renamed: write %s=%s", key, newKey, value)
		}
		isKey, isFlag := slices.Contains(s.Keys, key), slices.Contains(s.Flags, key)
		switch {
		case key == "":
			return nil, fmt.Errorf("invalid marker option %q (known: %s)", token, s.known())
		case !isKey && !isFlag:
			return nil, fmt.Errorf("unknown marker option %q in %s%s (known: %s)", key, Prefix, s.Kind, s.known())
		case hasValue && !isKey:
			return nil, fmt.Errorf("invalid marker option %q: %s is a bare flag, write %s%s %s", token, key, Prefix, s.Kind, key)
		case !hasValue && !isFlag:
			return nil, fmt.Errorf("invalid marker option %q: want key=value, e.g. %s=<value> (known: %s)", token, key, s.known())
		}
		if _, dup := options[key]; dup {
			return nil, fmt.Errorf("duplicate marker option %q", key)
		}
		if !hasValue {
			value = "true"
		}
		options[key] = value
	}
	return options, nil
}

// cutKind returns the options after "//roost:<kind>" when line is exactly
// that marker: "//roost:protocol" is not a "proto" marker and "//roost:daox"
// is not a "dao" marker.
func cutKind(line, kind string) (string, bool) {
	rest, ok := Cut(line, kind)
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return rest, true
}

// CheckFile validates the options of every marker of the given kinds in
// file and reports the first bad one as "path:line: ...". Generators call it
// right after parsing a source file, before they read any option, so a typo
// fails the run instead of turning into a default.
func CheckFile(fset *token.FileSet, file *ast.File, specs ...Spec) error {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			for _, spec := range specs {
				body, ok := cutKind(comment.Text, spec.Kind)
				if !ok {
					continue
				}
				if _, err := spec.Parse(body); err != nil {
					position := fset.Position(comment.Pos())
					return fmt.Errorf("%s:%d: %w", position.Filename, position.Line, err)
				}
			}
		}
	}
	return nil
}
