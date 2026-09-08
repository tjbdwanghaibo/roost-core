package skill

import "testing"

func TestCompileEnvironmentRejectsEmptyAuthority(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Revision = ""
	diagnostics := validateCompileEnvironment(environment)
	requireDiagnostic(t, diagnostics, DiagnosticEnvironmentInvalid)
}

func TestAuthorityDigestChangesWithElementPolicy(t *testing.T) {
	a := DefaultCompileEnvironment()
	b := DefaultCompileEnvironment()
	b.Gameplay.Elements = b.Gameplay.Elements.WithTestRevision("changed")
	if authorityDigest(a) == authorityDigest(b) {
		t.Fatal("authority digest must cover gameplay catalogs")
	}
}

func TestVisualCatalogDoesNotChangeGameplayAuthorityDigest(t *testing.T) {
	a := DefaultCompileEnvironment()
	b := DefaultCompileEnvironment()
	b.Visual = b.Visual.WithTestRevision("changed")
	if authorityDigest(a) != authorityDigest(b) {
		t.Fatal("visual identity must be separate from gameplay authority")
	}
}

func TestAuthorityIdentityRequiresExactNonEmptyMatch(t *testing.T) {
	want := AuthorityIdentity{Revision: "gameplay-1", Digest: "abc"}
	for _, tt := range []struct {
		name string
		got  AuthorityIdentity
		ok   bool
	}{
		{name: "exact", got: want, ok: true},
		{name: "empty revision", got: AuthorityIdentity{Digest: "abc"}},
		{name: "empty digest", got: AuthorityIdentity{Revision: "gameplay-1"}},
		{name: "different revision", got: AuthorityIdentity{Revision: "gameplay-2", Digest: "abc"}},
		{name: "different digest", got: AuthorityIdentity{Revision: "gameplay-1", Digest: "def"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorityMatches(want, tt.got); got != tt.ok {
				t.Fatalf("authorityMatches() = %v, want %v", got, tt.ok)
			}
		})
	}
}

func TestGameplayCatalogValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompileEnvironment)
		code   DiagnosticCode
	}{
		{
			name: "duplicate attribute handle",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Attributes.Entries = append(environment.Gameplay.Attributes.Entries, environment.Gameplay.Attributes.Entries[0])
			},
			code: DiagnosticCatalogDuplicateHandle,
		},
		{
			name: "missing neutral element",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Elements.Entries = environment.Gameplay.Elements.Entries[1:]
			},
			code: DiagnosticCatalogNeutralElement,
		},
		{
			name: "tag without declaration class",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Tags.Entries[0].Classes = 0
			},
			code: DiagnosticCatalogTagClass,
		},
		{
			name: "status references unknown attribute",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Statuses.Entries[0].AttributeModifiers[0].Attribute = AttributeHandle(65000)
			},
			code: DiagnosticCatalogReference,
		},
		{
			name: "attribute missing quantity",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Attributes.Entries[0].Quantity = quantityUnknown
			},
			code: DiagnosticCatalogAttributePolicy,
		},
		{
			name: "combat policy unknown damage type",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Combat.DamageTypes = append(environment.Gameplay.Combat.DamageTypes, DamageTypeHandle(65000))
			},
			code: DiagnosticCatalogReference,
		},
		{
			name:   "limits below catalog minimum",
			mutate: func(environment *CompileEnvironment) { environment.Limits.MaxGameplayTags = 0 },
			code:   DiagnosticEnvironmentLimits,
		},
		{
			name: "shared state missing lifetime",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.SharedStates.Entries[0].MaximumDurationTicks = 0
			},
			code: DiagnosticCatalogStatePolicy,
		},
		{
			name: "ability property missing mutation bound",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.Abilities.Properties[0].MaximumMutation = 0
			},
			code: DiagnosticCatalogAbilityPolicy,
		},
		{
			name: "unit template missing control profile",
			mutate: func(environment *CompileEnvironment) {
				environment.Gameplay.UnitTemplates.Entries[0].ControlProfile = ""
			},
			code: DiagnosticCatalogUnitPolicy,
		},
		{
			name:   "temporal profile missing fields",
			mutate: func(environment *CompileEnvironment) { environment.Gameplay.Temporal.Entries[0].Fields = nil },
			code:   DiagnosticCatalogTemporalPolicy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			tt.mutate(&environment)
			requireDiagnostic(t, validateCompileEnvironment(environment), tt.code)
		})
	}
}

func TestCompileEnvironmentDiagnosticsAreStable(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Revision = ""
	environment.Digest = ""
	environment.Gameplay.Elements.Entries = nil
	diagnostics := validateCompileEnvironment(environment)
	for index := 1; index < len(diagnostics); index++ {
		if diagnosticLess(diagnostics[index], diagnostics[index-1]) {
			t.Fatalf("diagnostics not sorted: %#v", diagnostics)
		}
	}
}

func requireDiagnostic(t *testing.T, diagnostics []Diagnostic, code DiagnosticCode) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %s in %#v", code, diagnostics)
}
