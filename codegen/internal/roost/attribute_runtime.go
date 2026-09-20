package roost

import "strings"

// attributeRuntimeTemplate is the file the attribute feature writes into the
// project's attribute package. It re-exports the framework runtime
// (roost-core/attribute) under the names the generator's output uses, so a
// project that declares a profile compiles without writing any of it by hand.
//
// Aliases rather than definitions, and the generator emits package-level
// accessors rather than methods on Snapshot / Container — the two decisions
// are the same decision: Go forbids methods on a type from another package,
// so as long as the generated code hangs no method on them, these can be the
// framework's own types and a profile is portable between projects
// (RR-20260917-06).
const attributeRuntimeTemplate = `` + generatedHeader + `
package PACKAGE

// The attribute feature's framework half, re-exported under the names the
// generated profiles use. Regenerate with ` + "`roost project sync`" + `.
//
// What a project writes by hand is the profile struct, its ` + "`attr`" + ` tags and
// its derived formulas; everything else — ids, masks, metadata, typed
// setters, Update, and the typed accessors below — is generated from the
// ` + "`//roost:attribute`" + ` marker.
//
// One field the declaration must carry itself:
//
//	type CombatProfile struct {
//	    HP int64 ` + "`attr:\"hp\"`" + `
//	    dirtyMask uint64 // every generated setter ORs its bit in here
//	}
//
// The generator writes through ` + "`dirtyMask`" + `; a profile without it does not
// compile, and the generator says so rather than leaving you to guess.

import coreattribute "github.com/tjbdwanghaibo/roost-core/attribute"

type (
	// AttrID identifies one attribute inside a profile.
	AttrID = coreattribute.AttrID
	// AttrValue is the numeric form every attribute travels in.
	AttrValue = coreattribute.AttrValue
	// AttributeMeta describes one attribute: names, dirty bit, derived flag.
	AttributeMeta = coreattribute.Meta
	// AttributeProfile is what a generated profile implements.
	AttributeProfile = coreattribute.Profile
	// Selector names one layer of a subject's attributes.
	Selector = coreattribute.Selector
	// Snapshot is one layer read at one moment; its profile is a copy.
	Snapshot = coreattribute.Snapshot
	// Container holds a subject's layers.
	Container = coreattribute.Container
)

// Base and Final are the two layers every game has under some spelling; a
// game with more names them itself.
var (
	Base  = coreattribute.Base
	Final = coreattribute.Final
)

// NewContainer builds an empty container for one subject.
func NewContainer() *Container { return coreattribute.NewContainer() }
`

// renderAttributeRuntime returns the runtime file for a package name.
func renderAttributeRuntime(pkg string) string {
	return strings.Replace(attributeRuntimeTemplate, "package PACKAGE", "package "+pkg, 1)
}

// AttributeRuntimeFile is renderAttributeRuntime for tooling outside this
// package — scripts/attribute-runtime.sh compiles exactly what the scaffold
// writes rather than a copy that can drift.
func AttributeRuntimeFile(pkg string) string { return renderAttributeRuntime(pkg) }
