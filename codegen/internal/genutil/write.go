// Package genutil holds the small helpers every generator shares.
package genutil

import (
	"bytes"
	"os"
)

// WriteIfChanged writes content to path only when it differs from what is on
// disk, so an idempotent regeneration leaves file contents and mtimes alone.
// force skips the comparison and always writes. Returns whether a write
// happened.
func WriteIfChanged(path string, content []byte, force bool) (bool, error) {
	if !force {
		existing, err := os.ReadFile(path)
		if err == nil && bytes.Equal(existing, content) {
			return false, nil
		}
	}
	return true, os.WriteFile(path, content, 0644)
}
