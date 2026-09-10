package entity

import (
	"errors"
	"strings"
	"testing"
)

func expectErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// U-0099 (C2): the kind/category registry is the root of every entity id in
// the system, and none of its refusals had a test. Each rule is pinned by the
// reason it names; re-registering an identical pair stays idempotent.
func TestRegisterEntityKindCategoryRefusesEachInvalidPair(t *testing.T) {
	const kind EntityKind = 201
	expectErrContains(t, RegisterEntityKindCategory(EntityKindNone, 1), "entity kind must not be none")
	expectErrContains(t, RegisterEntityKindCategory(kind, EntityCategoryNone), "entity category must not be none for kind 201")
	// A category above the ID's two-bit field used to be refused. M-02 made the
	// registry the authority on a kind's category and demoted that field to
	// legacy padding, so the bound is gone: the target taxonomy needs five
	// categories and the field can express three. Checked on its own kind so
	// the pair rules below still run against a fresh one.
	const aboveField EntityKind = 231
	if err := RegisterEntityKindCategory(aboveField, EntityCategory(EntityCategoryMask)+1); err != nil {
		t.Fatalf("category above the legacy field must now be accepted: %v", err)
	}
	if got, ok := EntityCategoryOfKind(aboveField); !ok || got != EntityCategory(EntityCategoryMask)+1 {
		t.Fatalf("EntityCategoryOfKind = (%d, %v) after registering above the field", got, ok)
	}
	if _, ok := EntityCategoryOfKind(kind); ok {
		t.Fatal("a refused registration left the kind registered")
	}
	if err := RegisterEntityKindCategory(kind, 1); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEntityKindCategory(kind, 1); err != nil {
		t.Fatalf("identical re-registration must be idempotent: %v", err)
	}
	expectErrContains(t, RegisterEntityKindCategory(kind, 2), "entity kind 201 category mismatch: registered=1 new=2")
	if category, _ := EntityCategoryOfKind(kind); category != 1 {
		t.Fatalf("a refused re-registration changed the category to %d", category)
	}
}

func TestResolveEntityKindCategoryRefusesNoneAndUnregistered(t *testing.T) {
	expectErrContains(t, func() error { _, err := ResolveEntityKindCategory(EntityKindNone); return err }(), "entity kind must not be none")
	_, err := ResolveEntityKindCategory(EntityKind(202))
	if !errors.Is(err, ErrInvalidEntityID) || !strings.Contains(err.Error(), "kind 202 category is not registered") {
		t.Fatalf("unregistered kind err = %v", err)
	}
}

// NormalizeID is where a create param's kind, category and id are reconciled
// against the registry before any builder runs.
func TestEntityCreateParamNormalizeIDRefusesEachInconsistency(t *testing.T) {
	const kind, other EntityKind = 203, 204
	MustRegisterEntityKindCategory(kind, 1)
	MustRegisterEntityKindCategory(other, 1)

	var nilParam *EntityCreateParam
	expectErrContains(t, nilParam.NormalizeID(kind), "entity create param is nil")
	expectErrContains(t, (&EntityCreateParam{}).NormalizeID(EntityKindNone), "entity kind must not be none")
	expectErrContains(t, (&EntityCreateParam{Kind: kind}).NormalizeID(other), "entity kind mismatch: param=203 builder=204")
	expectErrContains(t, (&EntityCreateParam{Kind: kind, Category: 2}).NormalizeID(kind), "entity category mismatch: param=2 kind=203 category=1")
	expectErrContains(t, (&EntityCreateParam{Kind: kind}).NormalizeID(kind), "entity load requires full Id or UniqueID")
	if err := (&EntityCreateParam{Kind: kind, IsCreate: true}).NormalizeID(kind); !errors.Is(err, ErrIDGeneratorRequired) {
		t.Fatalf("create without an id err = %v, want ErrIDGeneratorRequired", err)
	}

	param := &EntityCreateParam{UniqueID: 42}
	if err := param.NormalizeID(kind); err != nil {
		t.Fatal(err)
	}
	want, _ := BuildEntityID(42, kind)
	if param.Id != want || param.Kind != kind || param.Category != 1 {
		t.Fatalf("normalized param = %+v, want id %d kind %d category 1", param, want, kind)
	}
}

func TestResolveEntityBuilderRefusesMissingKindAndBuilder(t *testing.T) {
	const kind EntityKind = 205
	MustRegisterEntityKindCategory(kind, 1)
	_, err := resolveEntityBuilder(nil)
	expectErrContains(t, err, "entity create param is nil")
	_, err = resolveEntityBuilder(&EntityCreateParam{})
	expectErrContains(t, err, "entity kind must not be none")
	_, err = resolveEntityBuilder(&EntityCreateParam{Kind: kind, Category: 1})
	expectErrContains(t, err, "no entity builder registered for category 1 kind 205")
}
