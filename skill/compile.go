package skill

import "sort"

func compileToArtifacts(definition *Definition, environment CompileEnvironment) (*compileArtifacts, []Diagnostic) {
	artifacts, diagnostics := compileToArtifactsInternal(definition, environment)
	if diagnosticsHaveErrors(diagnostics) {
		return nil, diagnostics
	}
	return artifacts, diagnostics
}

func compileToArtifactsInternal(definition *Definition, environment CompileEnvironment) (*compileArtifacts, []Diagnostic) {
	artifacts := &compileArtifacts{}
	context := &compileContext{definition: definition, environment: environment, artifacts: artifacts}
	passes := []compilePass{
		compilePassFunc{"normalize", runNormalizePass},
		compilePassFunc{"shape", runShapePass},
		compilePassFunc{"authority_capability", runAuthorityCapabilityPass},
		compilePassFunc{"gameplay_tags", runGameplayTagsPass},
		compilePassFunc{"input_state", runInputStatePass},
		compilePassFunc{"temporal", runTemporalPass},
		compilePassFunc{"type_snapshot", runTypeSnapshotPass},
		compilePassFunc{"optional_quantity", runOptionalQuantityPass},
		compilePassFunc{"effect_result_scope", runEffectResultScopePass},
		compilePassFunc{"graph", runGraphPass},
		compilePassFunc{"memory", runMemoryPass},
		compilePassFunc{"lifetime_ownership", runLifetimePass},
		compilePassFunc{"motion", runMotionPass},
		compilePassFunc{"event_proc", runEventProcPass},
		compilePassFunc{"identity_random", runIdentityRandomPass},
		compilePassFunc{"budget", runBudgetPass},
		compilePassFunc{"visual", runVisualPass},
		compilePassFunc{"lower", runLowerReadinessPass},
	}
	for _, pass := range passes {
		artifacts.passOrder = append(artifacts.passOrder, pass.name())
		pass.run(context)
		if context.hasErrors() {
			break
		}
	}
	sort.SliceStable(context.diagnostics, func(i, j int) bool { return diagnosticLess(context.diagnostics[i], context.diagnostics[j]) })
	return artifacts, append([]Diagnostic(nil), context.diagnostics...)
}

func diagnosticsHaveErrors(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticError {
			return true
		}
	}
	return false
}

func runNormalizePass(context *compileContext) {
	ir, sources, diagnostics := normalizeDefinition(context.definition)
	context.artifacts.ir = ir
	context.artifacts.sources = sources
	context.artifacts.metadata = compileMetadata{
		CompilerSemanticsRevision: context.environment.CompilerSemanticsRevision,
		SourceDocumentDigest:      canonicalDefinitionDigest(context.definition),
		VisualRevision:            context.environment.Visual.Revision,
		VisualDigest:              context.environment.Visual.Digest,
	}
	context.diagnostics = append(context.diagnostics, diagnostics...)
}
func runOptionalQuantityPass(*compileContext)       {}
func runLowerReadinessPass(context *compileContext) { context.artifacts.lowerReady = true }
