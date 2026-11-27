// Package entitypermit supplies the capability for framework-owned native entity
// lock operations. It is internal to framework: application packages can use the
// entity facade, but cannot request this capability.
package entitypermit

// Token is passed explicitly. Its zero value grants nothing.
type Token struct{ system bool }

// System returns the capability for a framework operation that needs the native lock.
func System() Token { return Token{system: true} }

// Valid reports whether the token was issued by System.
func (t Token) Valid() bool { return t.system }
