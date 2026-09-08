package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type AuthorityIdentity struct {
	Revision string
	Digest   string
}
type AuthorityProvider interface{ AuthorityIdentity() AuthorityIdentity }

func authorityMatches(expected, actual AuthorityIdentity) bool {
	return expected.Revision != "" && expected.Digest != "" && actual.Revision != "" && actual.Digest != "" && expected == actual
}

func authorityDigest(environment CompileEnvironment) string {
	payload := struct {
		Domain, Revision  string
		Limits            CompileLimits
		Numeric           NumericAuthority
		Gameplay          GameplayCatalog
		Motion            MotionCapabilityCatalog
		ProcessProperties ProcessPropertyCatalog
	}{Domain: "roost.skill/v2/gameplay-authority", Revision: environment.Revision, Limits: environment.Limits, Numeric: environment.Numeric, Gameplay: environment.Gameplay, Motion: environment.Motion, ProcessProperties: environment.ProcessProperties}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestStrings(domain, revision string, values []string) string {
	data, err := json.Marshal(struct {
		Domain, Revision string
		Values           []string
	}{domain, revision, values})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateCompileEnvironment(environment CompileEnvironment) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	add := func(code DiagnosticCode, path, message string) {
		diagnostics = append(diagnostics, Diagnostic{Code: code, Severity: DiagnosticError, Path: path, Message: message})
	}
	if environment.CompilerSemanticsRevision == "" {
		add(DiagnosticEnvironmentInvalid, "$.compiler_semantics_revision", "compiler semantics revision is required")
	}
	if environment.Revision == "" {
		add(DiagnosticEnvironmentInvalid, "$.revision", "authority revision is required")
	}
	if environment.Digest == "" {
		add(DiagnosticEnvironmentInvalid, "$.digest", "authority digest is required")
	} else if environment.Digest != authorityDigest(environment) {
		add(DiagnosticEnvironmentInvalid, "$.digest", "authority digest does not match environment")
	}
	validateAttributes(environment.Gameplay.Attributes, &diagnostics)
	validateElements(environment.Gameplay.Elements, &diagnostics)
	validateTags(environment.Gameplay.Tags, &diagnostics)
	validateCatalogHandles(environment, &diagnostics)
	validateReferences(environment.Gameplay, &diagnostics)
	validatePolicies(environment, &diagnostics)
	validateMotionCatalog(environment, &diagnostics)
	validateProcessPropertyCatalog(environment.ProcessProperties, &diagnostics)
	sort.SliceStable(diagnostics, func(i, j int) bool { return diagnosticLess(diagnostics[i], diagnostics[j]) })
	return diagnostics
}

func diagnosticLess(a, b Diagnostic) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Severity != b.Severity {
		return a.Severity < b.Severity
	}
	if a.Code != b.Code {
		return a.Code < b.Code
	}
	return a.Message < b.Message
}

func validateAttributes(catalog AttributeCatalog, diagnostics *[]Diagnostic) {
	for index, entry := range catalog.Entries {
		path := fmt.Sprintf("$.gameplay.attributes[%d]", index)
		if entry.Key == "" || entry.ValueType == valueKindInvalid || entry.Quantity == quantityUnknown || entry.Minimum > entry.Maximum || entry.Rounding == "" || len(entry.Snapshots) == 0 {
			appendDiagnostic(diagnostics, DiagnosticCatalogAttributePolicy, path, "attribute type, quantity, range, rounding, and snapshots are required")
		}
	}
}

func validateElements(catalog ElementCatalog, diagnostics *[]Diagnostic) {
	if len(catalog.Entries) == 0 || catalog.Entries[0].Key != "neutral" {
		appendDiagnostic(diagnostics, DiagnosticCatalogNeutralElement, "$.gameplay.elements", "first element must be neutral")
	}
}
func validateTags(catalog GameplayTagCatalog, diagnostics *[]Diagnostic) {
	for index, entry := range catalog.Entries {
		if entry.Classes == 0 {
			appendDiagnostic(diagnostics, DiagnosticCatalogTagClass, fmt.Sprintf("$.gameplay.tags[%d]", index), "tag declaration class is required")
		}
	}
}

func validateCatalogHandles(environment CompileEnvironment, diagnostics *[]Diagnostic) {
	checkHandles("$.gameplay.attributes", len(environment.Gameplay.Attributes.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Attributes.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.resources", len(environment.Gameplay.Resources.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Resources.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.statuses", len(environment.Gameplay.Statuses.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Statuses.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.unit_templates", len(environment.Gameplay.UnitTemplates.Entries), func(i int) uint16 { return uint16(environment.Gameplay.UnitTemplates.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.collision", len(environment.Gameplay.Collision.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Collision.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.damage_types", len(environment.Gameplay.DamageTypes.Entries), func(i int) uint16 { return uint16(environment.Gameplay.DamageTypes.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.elements", len(environment.Gameplay.Elements.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Elements.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.tags", len(environment.Gameplay.Tags.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Tags.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.shared_states", len(environment.Gameplay.SharedStates.Entries), func(i int) uint16 { return uint16(environment.Gameplay.SharedStates.Entries[i].Handle) }, diagnostics)
	checkHandles("$.gameplay.temporal", len(environment.Gameplay.Temporal.Entries), func(i int) uint16 { return uint16(environment.Gameplay.Temporal.Entries[i].Handle) }, diagnostics)
	checkHandles("$.process_properties", len(environment.ProcessProperties.Properties), func(i int) uint16 { return uint16(environment.ProcessProperties.Properties[i].Handle) }, diagnostics)
}

func validateProcessPropertyCatalog(catalog ProcessPropertyCatalog, diagnostics *[]Diagnostic) {
	allowedKeys := map[string]bool{
		"speed": true, "radius": true, "arc_height": true, "turn_rate_mdeg_per_tick": true,
		"angular_speed_mdeg_per_tick": true, "offset_amplitude": true, "offset_radius": true,
		"return_speed_bp": true, "collision_force": true,
	}
	canonical := make(map[string]ProcessPropertyPolicy)
	for _, policy := range defaultProcessPropertyCatalog().Properties {
		canonical[policy.Key] = policy
	}
	seenKeys := make(map[string]bool)
	if catalog.Revision == "" {
		appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, "$.process_properties.revision", "process property catalog revision is required")
	}
	for index, policy := range catalog.Properties {
		path := fmt.Sprintf("$.process_properties.properties[%d]", index)
		valid := allowedKeys[policy.Key] && !seenKeys[policy.Key] && policy.Minimum <= policy.Maximum && len(policy.ProcessKinds) > 0 && uniqueNonEmptyStrings(policy.ProcessKinds) && len(policy.Operations) > 0 && uniqueNonEmptyStrings(policy.Operations) && policy.Interpolation == "linear_integer" && policy.Rounding == "truncate_toward_zero" && len(policy.SlotBindings) > 0
		for _, operation := range policy.Operations {
			valid = valid && (operation == "set" || operation == "add" || operation == "mul_bp")
		}
		for _, processKind := range policy.ProcessKinds {
			valid = valid && validMotionProcessKind(processKind)
		}
		for _, binding := range policy.SlotBindings {
			valid = valid && binding.Stage != "" && binding.Variant != "" && binding.Field != ""
		}
		expected, canonicalPolicy := canonical[policy.Key]
		valid = valid && canonicalPolicy && equalProcessPropertyPolicy(policy, expected)
		if !valid {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, path, "process property must exactly match its canonical handle, range, process kinds, operations, interpolation, rounding, and slot bindings")
		}
		seenKeys[policy.Key] = true
	}
	for key := range canonical {
		if !seenKeys[key] {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, "$.process_properties.properties", "process property catalog must contain every canonical numeric property")
			break
		}
	}
}

func equalProcessPropertyPolicy(left, right ProcessPropertyPolicy) bool {
	if left.Handle != right.Handle || left.Key != right.Key || left.Minimum != right.Minimum || left.Maximum != right.Maximum || left.Interpolation != right.Interpolation || left.Rounding != right.Rounding || !equalStrings(left.ProcessKinds, right.ProcessKinds) || !equalStrings(left.Operations, right.Operations) || len(left.SlotBindings) != len(right.SlotBindings) {
		return false
	}
	for index := range left.SlotBindings {
		if left.SlotBindings[index] != right.SlotBindings[index] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func checkHandles(path string, length int, handle func(int) uint16, diagnostics *[]Diagnostic) {
	seen := map[uint16]bool{}
	for i := 0; i < length; i++ {
		value := handle(i)
		if value == 0 || seen[value] {
			appendDiagnostic(diagnostics, DiagnosticCatalogDuplicateHandle, fmt.Sprintf("%s[%d].handle", path, i), "handle must be non-zero and unique")
		}
		seen[value] = true
	}
}

func validateReferences(catalog GameplayCatalog, diagnostics *[]Diagnostic) {
	attributes := map[AttributeHandle]bool{}
	attributePolicies := map[AttributeHandle]AttributeCatalogEntry{}
	for _, entry := range catalog.Attributes.Entries {
		attributes[entry.Handle] = true
		attributePolicies[entry.Handle] = entry
	}
	tags := map[GameplayTagHandle]bool{}
	for _, entry := range catalog.Tags.Entries {
		tags[entry.Handle] = true
	}
	damageTypes := map[DamageTypeHandle]bool{}
	for _, entry := range catalog.DamageTypes.Entries {
		damageTypes[entry.Handle] = true
	}
	for i, status := range catalog.Statuses.Entries {
		for j, hook := range status.CombatHooks {
			if !validCombatHook(hook) {
				appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.statuses[%d].combat_hooks[%d]", i, j), "unknown combat hook handle")
			}
		}
		for j, modifier := range status.AttributeModifiers {
			if !attributes[modifier.Attribute] {
				appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.statuses[%d].attribute_modifiers[%d].attribute", i, j), "unknown attribute handle")
			}
		}
		for j, tag := range append(append([]GameplayTagHandle(nil), status.ImmunityTags...), status.GameplayTags...) {
			if !tags[tag] {
				appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.statuses[%d].tags[%d]", i, j), "unknown tag handle")
			}
		}
	}
	for index, handle := range catalog.Combat.DamageTypes {
		if !damageTypes[handle] {
			appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.combat.damage_types[%d]", index), "unknown damage type handle")
		}
	}
	for i, template := range catalog.UnitTemplates.Entries {
		for j, tag := range template.GameplayTags {
			if !tags[tag] {
				appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.unit_templates[%d].gameplay_tags[%d]", i, j), "unknown gameplay tag handle")
			}
		}
		for j, override := range template.AllowedAttributeOverrides {
			attribute := attributePolicies[override.Attribute]
			if !attributes[override.Attribute] || override.Minimum > override.Maximum || override.Minimum < attribute.Minimum || override.Maximum > attribute.Maximum {
				appendDiagnostic(diagnostics, DiagnosticCatalogReference, fmt.Sprintf("$.gameplay.unit_templates[%d].allowed_attribute_overrides[%d]", i, j), "unknown attribute handle or invalid clamp bounds")
			}
		}
	}
}

func validCombatHook(hook string) bool {
	switch hook {
	case "damage_redirect", "damage_share", "death_prevention", "spell_shield", "reflect_damage", "lifesteal", "omnivamp", "heal_reduction", "damage_amplification", "critical_override", "control_immunity", "tenacity", "execute_immunity":
		return true
	default:
		return false
	}
}

func validatePolicies(environment CompileEnvironment, diagnostics *[]Diagnostic) {
	if environment.Limits.MaxGameplayTags < len(environment.Gameplay.Tags.Entries) || environment.Limits.MaxPhases <= 0 || environment.Limits.MaxFlowNodes <= 0 || environment.Limits.MaxLocalFrames <= 0 || environment.Limits.MaxRandomSites <= 0 || environment.Limits.MaxPassiveActivationsPerTick <= 0 {
		appendDiagnostic(diagnostics, DiagnosticEnvironmentLimits, "$.limits", "limits must cover catalog and runtime minimums")
	}
	for index, entry := range environment.Gameplay.SharedStates.Entries {
		if entry.Scope == "" || entry.MaximumDurationTicks <= 0 {
			appendDiagnostic(diagnostics, DiagnosticCatalogStatePolicy, fmt.Sprintf("$.gameplay.shared_states[%d]", index), "state scope and bounded lifetime are required")
		}
	}
	tagClasses := make(map[GameplayTagHandle]GameplayTagClass)
	for _, tag := range environment.Gameplay.Tags.Entries {
		tagClasses[tag.Handle] = tag.Classes
	}
	selectableTags := make(map[GameplayTagHandle]bool)
	for index, handle := range environment.Gameplay.Abilities.SelectableTags {
		if handle == 0 || selectableTags[handle] || tagClasses[handle]&GameplayTagTargetQueryable == 0 {
			appendDiagnostic(diagnostics, DiagnosticCatalogAbilityPolicy, fmt.Sprintf("$.gameplay.abilities.selectable_tags[%d]", index), "ability selectable tag must be unique and target-queryable")
		}
		selectableTags[handle] = true
	}
	relations := make(map[string]bool)
	for index, relation := range environment.Gameplay.Abilities.OwnerRelations {
		if (relation != "self" && relation != "ally" && relation != "enemy") || relations[relation] {
			appendDiagnostic(diagnostics, DiagnosticCatalogAbilityPolicy, fmt.Sprintf("$.gameplay.abilities.owner_relations[%d]", index), "ability owner relation must be unique and self, ally, or enemy")
		}
		relations[relation] = true
	}
	if len(environment.Gameplay.Abilities.OwnerRelations) == 0 {
		appendDiagnostic(diagnostics, DiagnosticCatalogAbilityPolicy, "$.gameplay.abilities.owner_relations", "at least one ability owner relation is required")
	}
	abilityProperties := make(map[string]bool)
	for index, entry := range environment.Gameplay.Abilities.Properties {
		expectedType, known := abilityPropertyValueKind(entry.Property)
		readOnly := entry.Property == "cooldown_total_ticks" || entry.Property == "ammo_max_stock" || entry.Property == "cast_active" || entry.Property == "last_commit_tick" || entry.Property == "last_finish_tick"
		if entry.Property == "" || !known || abilityProperties[entry.Property] || entry.ValueType != expectedType || entry.Minimum > entry.Maximum || (entry.Mutable && entry.MaximumMutation <= 0) || (readOnly && entry.Mutable) || (entry.Property == "enabled" && entry.Mutable && entry.MaximumDurationTicks <= 0) {
			appendDiagnostic(diagnostics, DiagnosticCatalogAbilityPolicy, fmt.Sprintf("$.gameplay.abilities.properties[%d]", index), "ability property bounds are required")
		}
		abilityProperties[entry.Property] = true
	}
	for index, entry := range environment.Gameplay.UnitTemplates.Entries {
		if entry.OwnerPolicy == "" || entry.MaximumPerOwner <= 0 || entry.MaximumPerSourceSkill <= 0 || entry.MaximumPerTeam <= 0 || entry.MaximumSpawnCount <= 0 || entry.MaximumLifetimeTicks <= 0 || entry.ReplacementPolicy == "" || entry.ControlProfile == "" || !validOwnedReplacementPolicy(entry.ReplacementPolicy) || !validOwnedLifecyclePolicy(entry.OwnerDeathPolicy) || !validOwnedLifecyclePolicy(entry.SkillRemovedPolicy) || !validOwnedLifecyclePolicy(entry.MatchEndPolicy) {
			appendDiagnostic(diagnostics, DiagnosticCatalogUnitPolicy, fmt.Sprintf("$.gameplay.unit_templates[%d]", index), "unit ownership, limit, replacement, and control profile are required")
		}
		if entry.MaximumPerOwner > environment.Limits.MaxOwnedEntities || entry.MaximumPerSourceSkill > environment.Limits.MaxOwnedEntities || entry.MaximumPerTeam > environment.Limits.MaxOwnedEntities || entry.MaximumSpawnCount > environment.Limits.MaxOwnedEntities || entry.MaximumLifetimeTicks > environment.Limits.MaxLifetimeTicks || !validOwnedCommandSet(entry.Commands) || !uniqueNonEmptyStrings(entry.Behaviors) || !validUnitTemplateParameters(entry.Parameters) {
			appendDiagnostic(diagnostics, DiagnosticCatalogUnitPolicy, fmt.Sprintf("$.gameplay.unit_templates[%d]", index), "unit limits, commands, and behaviors must be bounded closed sets")
		}
	}
	temporalKeys := make(map[string]bool, len(environment.Gameplay.Temporal.Entries))
	for index, entry := range environment.Gameplay.Temporal.Entries {
		if entry.Key == "" || temporalKeys[entry.Key] || len(entry.Fields) == 0 || entry.MaximumAgeTicks <= 0 || entry.MaximumAgeTicks > environment.Limits.MaxTemporalSnapshotAge || entry.MaximumPerOwner <= 0 || entry.MaximumPerOwner > environment.Limits.MaxTemporalSnapshots || entry.RestorePolicy != "authorized_fields" || !validTemporalEventPolicy(entry.EventPolicy) || !validTemporalBlockedPositionPolicy(entry.BlockedPositionPolicy) || !validTemporalFields(entry.Fields) {
			appendDiagnostic(diagnostics, DiagnosticCatalogTemporalPolicy, fmt.Sprintf("$.gameplay.temporal[%d]", index), "temporal fields, age, restore, and event policy are required")
		}
		temporalKeys[entry.Key] = true
	}
}

func validateMotionCatalog(environment CompileEnvironment, diagnostics *[]Diagnostic) {
	catalog := environment.Motion
	if catalog.Revision == "" || catalog.MaximumSpeed <= 0 || catalog.MaximumDistance <= 0 || catalog.MaximumAngularSpeed <= 0 || catalog.MaximumTrackingTicks <= 0 || len(catalog.ProcessTrajectoryPairs) == 0 || len(catalog.VariantCapabilities) == 0 || !uniqueNonEmptyStrings(catalog.EnabledSlots) || !uniqueNonEmptyStrings(catalog.HostFeatures) {
		appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, "$.motion", "motion catalog requires revision, closed capabilities, and positive bounds")
	}
	pairs := make(map[string]bool, len(catalog.ProcessTrajectoryPairs))
	for index, pair := range catalog.ProcessTrajectoryPairs {
		key := pair.Process + ":" + pair.Trajectory
		if !validMotionProcessKind(pair.Process) || !validMotionTrajectoryKind(pair.Trajectory) || pair.Process == "summon" || pairs[key] {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, fmt.Sprintf("$.motion.process_trajectory_pairs[%d]", index), "process/trajectory capability must be a unique supported closed pair")
		}
		pairs[key] = true
	}
	variants := make(map[string]bool, len(catalog.VariantCapabilities))
	for index, capability := range catalog.VariantCapabilities {
		key := capability.Process + ":" + capability.Trajectory
		if !pairs[key] || variants[key] || !validMotionVariantList(capability.Frames, validMotionFrameVariant) || !validMotionVariantList(capability.Steering, validMotionSteeringVariant) || !validMotionVariantList(capability.Offsets, validMotionOffsetVariant) || !validMotionVariantList(capability.CollisionResponses, validMotionCollisionVariant) || !validMotionVariantList(capability.Completions, validMotionCompletionVariant) {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, fmt.Sprintf("$.motion.variant_capabilities[%d]", index), "motion variant capability must be a unique closed process/trajectory policy")
		}
		variants[key] = true
	}
	for key := range pairs {
		if !variants[key] {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, "$.motion.variant_capabilities", "every process/trajectory capability requires a closed stage-variant policy")
			break
		}
	}
	for index, slot := range catalog.EnabledSlots {
		if !validMotionSlot(slot) {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, fmt.Sprintf("$.motion.enabled_slots[%d]", index), "motion slot is not closed")
		}
	}
	for index, feature := range catalog.HostFeatures {
		if feature != "carry" {
			appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, fmt.Sprintf("$.motion.host_features[%d]", index), "motion host feature is not closed")
		}
	}
	if environment.Limits.MaxMotionOffsets <= 0 || environment.Limits.MaxReflects <= 0 || environment.Limits.MaxPierces <= 0 || environment.Limits.MaxCarryTargets <= 0 || catalog.MaximumTrackingTicks > environment.Limits.MaxLifetimeTicks {
		appendDiagnostic(diagnostics, DiagnosticCatalogMotionPolicy, "$.motion", "motion catalog bounds must fit compile limits")
	}
}

func validMotionVariantList(values []string, valid func(string) bool) bool {
	if len(values) == 0 {
		return true
	}
	if !uniqueNonEmptyStrings(values) {
		return false
	}
	for _, value := range values {
		if !valid(value) {
			return false
		}
	}
	return true
}

func validMotionFrameVariant(value string) bool    { return value == "world" || value == "follow" }
func validMotionSteeringVariant(value string) bool { return value == "fixed" || value == "tracking" }
func validMotionOffsetVariant(value string) bool   { return value == "zigzag" || value == "circular" }
func validMotionCollisionVariant(value string) bool {
	return value == "stop" || value == "reflect" || value == "pierce"
}
func validMotionCompletionVariant(value string) bool {
	return value == "end" || value == "pause_then_end" || value == "boomerang"
}

func validMotionTrajectoryKind(kind string) bool {
	switch kind {
	case "stationary", "linear", "path", "orbit", "parabola":
		return true
	default:
		return false
	}
}
func validMotionSlot(slot string) bool {
	switch slot {
	case "frame", "steering", "offsets", "collision", "carry", "completion":
		return true
	default:
		return false
	}
}

func validTemporalEventPolicy(policy string) bool {
	return policy == "temporal_only" || policy == "derived_combat"
}

func validTemporalBlockedPositionPolicy(policy string) bool {
	return policy == "fail" || policy == "nearest" || policy == "stay"
}

func validTemporalFields(fields []string) bool {
	allowed := map[string]bool{"position": true, "facing": true, "health": true, "resources": true, "statuses": true, "ability_state": true, "cooldowns": true}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !allowed[field] || seen[field] {
			return false
		}
		seen[field] = true
	}
	return true
}

func validOwnedLifecyclePolicy(policy string) bool {
	return policy == "despawn" || policy == "persist_until_duration"
}

func validUnitTemplateParameters(parameters []UnitTemplateParameterPolicy) bool {
	seen := make(map[string]bool, len(parameters))
	for _, parameter := range parameters {
		if parameter.Name == "" || seen[parameter.Name] || parameter.ValueType == valueKindInvalid || parameter.ValueType == valueKindInt && parameter.Quantity == quantityUnknown || parameter.Minimum > parameter.Maximum {
			return false
		}
		seen[parameter.Name] = true
	}
	return true
}

func validOwnedCommandSet(commands []string) bool {
	if !uniqueNonEmptyStrings(commands) {
		return false
	}
	for _, command := range commands {
		switch command {
		case "move_to", "follow", "attack_target", "hold_position", "return_to_owner", "stop", "invoke_behavior", "despawn":
		default:
			return false
		}
	}
	return true
}

func uniqueNonEmptyStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, found := seen[value]; found {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func appendDiagnostic(target *[]Diagnostic, code DiagnosticCode, path, message string) {
	*target = append(*target, Diagnostic{Code: code, Severity: DiagnosticError, Path: path, Message: message})
}
