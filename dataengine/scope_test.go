package dataengine

import "testing"

type scopedDocument struct{ scope DatabaseScope }

func (value scopedDocument) DbScope() DatabaseScope { return value.scope }

func TestResolveDatabaseScopeDefaultsAndUsesTypedScope(t *testing.T) {
	if got := ResolveDatabaseScope(nil); got != DatabaseGlobal {
		t.Fatalf("default scope=%d", got)
	}
	if got := ResolveDatabaseScope(scopedDocument{scope: DatabaseServer}); got != DatabaseServer {
		t.Fatalf("typed scope=%d", got)
	}
}

func TestMapPatchPathBuildsOnlySafeNestedPaths(t *testing.T) {
	if path, ok := MapPatchPath("items", int64(100)); !ok || path != "items.100" {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	for _, key := range []string{"bad.key", "$bad", "", "bad\x00key"} {
		if path, ok := MapPatchPath("items", key); ok {
			t.Fatalf("unsafe key %q produced %q", key, path)
		}
	}
}
