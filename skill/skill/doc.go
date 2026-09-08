// Package skill is roost-skill's stable public compiler and deterministic
// runtime API.
//
// Skill definitions use a separately versioned wire contract. The JSON
// schema is roost.skill/v2 (renamed from cube.skill/v2 in v1.10.0, before any
// data existed under the old name) and the compiler semantics revision is
// skillv2-compiler-2; neither value is the Go package version. Applications
// should import github.com/tjbdwanghaibo/roost-skill/skill and persist the wire
// and compiler identities emitted by the package instead of deriving them from
// its import path.
//
// The supported lifecycle is Parse, Compile, inspect the immutable Program,
// and execute it through Runtime against a Host implementation. Production
// hosts must honor the serialization, non-reentrancy, bounded-work, authority,
// and determinism contracts documented on Host and Runtime.
package skill
