// Package demo carries the `roost project new … -template game-demo` sources.
//
// They live here as real files, laid out exactly as they land in a generated
// project, so they can be read and reviewed as the code they become instead of
// as Go string literals. The `.tmpl` suffix keeps them out of this module's
// build: they import roost-core, and codegen deliberately does not depend on
// the runtime it generates for.
//
// Nothing here is compiled by this module. It is verified by CI generating a
// project from the template and building it — the same reason the framework's
// own examples/ module rotted is that nothing did that for it.
package demo

import "embed"

// Files holds every template, keyed by its path inside a generated project
// plus a `.tmpl` suffix. TestDemoEmbedCoversEveryFile keeps this pattern list
// honest: a new top-level directory that is not named here fails that test
// rather than silently shipping an empty template.
//
//go:embed cmd configs db deploy game internal loadtest protocol
var Files embed.FS
