// Package skillcompose builds and validates authority-bound contracts for
// composing compiled skill programs.
//
// It consumes only the stable inspector views exported by package skill and
// never executes a Runtime or mutates a Program. Callers should construct
// contracts with BuildContract, expose only DeriveContractPromptView to
// generators, and validate every generated candidate before compilation.
package skillcompose
