// Package split shows the two sides of a split deployment, and shows that the
// business code between them is the same code.
//
// It is a compiling example rather than a snippet in a document: an example
// that does not build is a document that has already drifted. Nothing here is
// meant to be imported — it exists so the wiring can be read and so the
// compiler checks it.
//
// The three files are the three things a deployment writes:
//
//	mailprocess.go   the process that OWNS mail
//	gameprocess.go   a process that CALLS mail
//	consumer.go      the business logic, identical in both
//
// The whole point is that consumer.go does not appear in either wiring file's
// decisions. It looks mail up by interface and does not know, and cannot
// depend on, which process answers.
package split
