package entity

import "testing"

func TestBusinessPoolDefinitionIsImmutableAndValidated(t *testing.T) {
	base := &entityKindEntry{category: 1, businessPool: BusinessPoolLong}
	if _, err := resolveEntityKindDefinition(base, EntityKindDef{Kind: 1, Category: 1, BusinessPool: BusinessPoolShort}); err == nil {
		t.Fatal("changed registered business pool")
	}
	if _, err := resolveEntityKindDefinition(nil, EntityKindDef{Kind: 1, Category: 1, BusinessPool: 99}); err == nil {
		t.Fatal("invalid pool accepted")
	}
	next, err := resolveEntityKindDefinition(base, EntityKindDef{Kind: 1, Category: 1})
	if err != nil || next.businessPool != BusinessPoolLong {
		t.Fatal("partial category registration lost pool")
	}
}
