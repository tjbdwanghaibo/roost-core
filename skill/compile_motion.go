package skill

import "fmt"

func (n *normalizer) normalizeSpawn(value *SpawnDefinition, path string) *spawnIR {
	if value == nil {
		return nil
	}
	result := &spawnIR{source: n.source(path), kind: value.Kind, durationTicks: value.DurationTicks, intervalTicks: value.IntervalTicks, emitLeaveOnStop: value.EmitLeaveOnStop, visual: normalizeVisual(value.Visual), numericTracks: make([]numericTrackIR, len(value.NumericTracks))}
	if value.Area != nil {
		area := n.normalizeSelect(*value.Area, path+".area")
		result.area = &area
	}
	for index, track := range value.NumericTracks {
		trackPath := fmt.Sprintf("%s.numeric_tracks[%d]", path, index)
		result.numericTracks[index] = numericTrackIR{source: n.source(trackPath), property: track.Property, operation: track.Operation, value: n.normalizeValue(track.Value, trackPath+".value"), overTicks: track.OverTicks}
	}
	if value.Motion != nil {
		result.motion = n.normalizeMotion(*value.Motion, path+".motion")
	}
	return result
}

func (n *normalizer) normalizeMotion(value MotionDefinition, path string) motionIR {
	result := &canonicalMotionIR{source: n.source(path), frame: n.normalizeMotionFrame(value.Frame, path+".frame"), steering: n.normalizeMotionSteering(value.Steering, path+".steering"), trajectory: n.normalizeTrajectory(value.Trajectory, path+".trajectory"), offsets: make([]motionOffsetIR, len(value.Offsets)), completion: n.normalizeCompletion(value.Completion)}
	for index, offset := range value.Offsets {
		result.offsets[index] = n.normalizeOffset(offset, fmt.Sprintf("%s.offsets[%d]", path, index))
	}
	if value.Collision != nil {
		result.collision = &motionCollisionIR{layers: append([]string(nil), value.Collision.Layers...), response: value.Collision.Response, maxReflects: value.Collision.MaxReflects, maxPierces: value.Collision.MaxPierces}
	}
	if value.Carry != nil {
		result.carry = &motionCarryIR{target: n.normalizeValue(value.Carry.Target, path+".carry.target")}
	}
	return result
}

func (n *normalizer) normalizeMotionFrame(value FrameDefinition, path string) motionFrameIR {
	switch typed := value.(type) {
	case WorldFrameDefinition:
		return worldFrameIR{}
	case FollowFrameDefinition:
		return followFrameIR{target: n.normalizeValue(typed.Target, path+".target")}
	default:
		n.error("NORMALIZE_MOTION_FRAME_VARIANT", path, fmt.Sprintf("unsupported frame %T", value))
		return worldFrameIR{}
	}
}
func (n *normalizer) normalizeMotionSteering(value SteeringDefinition, path string) motionSteeringIR {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case TrackingSteeringDefinition:
		return trackingSteeringIR{target: n.normalizeValue(typed.Target, path+".target"), durationTicks: typed.DurationTicks}
	default:
		n.error("NORMALIZE_MOTION_STEERING_VARIANT", path, fmt.Sprintf("unsupported steering %T", value))
		return nil
	}
}
func (n *normalizer) normalizeTrajectory(value TrajectoryDefinition, path string) motionTrajectoryIR {
	switch typed := value.(type) {
	case StationaryTrajectoryDefinition:
		return stationaryTrajectoryIR{}
	case LinearTrajectoryDefinition:
		return linearTrajectoryIR{speed: n.normalizeValue(typed.Speed, path+".speed")}
	case PathTrajectoryDefinition:
		return pathTrajectoryIR{points: n.normalizeValue(typed.Points, path+".points"), speed: n.normalizeValue(typed.Speed, path+".speed")}
	case OrbitTrajectoryDefinition:
		return orbitTrajectoryIR{anchor: n.normalizeValue(typed.Anchor, path+".anchor"), radius: n.normalizeValue(typed.Radius, path+".radius"), angularSpeed: n.normalizeValue(typed.AngularSpeed, path+".angular_speed")}
	case ParabolaTrajectoryDefinition:
		return parabolaTrajectoryIR{destination: n.normalizeValue(typed.Destination, path+".destination"), height: n.normalizeValue(typed.Height, path+".height"), durationTicks: typed.DurationTicks}
	default:
		n.error("NORMALIZE_MOTION_TRAJECTORY_VARIANT", path, fmt.Sprintf("unsupported trajectory %T", value))
		return stationaryTrajectoryIR{}
	}
}
func (n *normalizer) normalizeOffset(value OffsetDefinition, path string) motionOffsetIR {
	switch typed := value.(type) {
	case ZigzagOffsetDefinition:
		return zigzagOffsetIR{amplitude: n.normalizeValue(typed.Amplitude, path+".amplitude"), periodTicks: typed.PeriodTicks}
	case CircularOffsetDefinition:
		return circularOffsetIR{radius: n.normalizeValue(typed.Radius, path+".radius"), angularSpeed: n.normalizeValue(typed.AngularSpeed, path+".angular_speed")}
	default:
		n.error("NORMALIZE_MOTION_OFFSET_VARIANT", path, fmt.Sprintf("unsupported offset %T", value))
		return zigzagOffsetIR{}
	}
}
func (n *normalizer) normalizeCompletion(value CompletionDefinition) motionCompletionIR {
	switch typed := value.(type) {
	case EndCompletionDefinition:
		return endCompletionIR{}
	case PauseThenEndCompletionDefinition:
		return pauseThenEndCompletionIR{pauseTicks: typed.PauseTicks}
	case BoomerangCompletionDefinition:
		return boomerangCompletionIR{maxReturnTicks: typed.MaxReturnTicks}
	default:
		return endCompletionIR{}
	}
}

func runMotionPass(context *compileContext) {
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		effect, ok := flow.(*effectFlowIR)
		if !ok || effect.spawn == nil {
			return
		}
		validateSpawnMotion(context, effect.spawn)
	})
}

func validateSpawnMotion(context *compileContext, spawn *spawnIR) {
	path := spawn.source.Path
	if !validMotionSpawnKind(spawn.kind) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".kind", "spawn kind is not a closed motion kind")
		return
	}
	if spawn.kind == "summon" {
		// summon 衍生物的寿命是 spawn 效果的 duration_ticks（编译期要求为正）；运行期只在带
		// motion / area 的衍生物上读模板时长（spawn_owned.go startEntitySpawn），所以
		// summon 自己的 duration_ticks 与 area 成员字段从来不生效。以前这里直接返回、
		// 负数也照样编译，写了 area 运行期反而拿 0 时长报 ErrProgramInvariant；现在写了
		// 就拒绝（O22，维护者第十二轮决定）。
		if spawn.motion != nil {
			context.addDiagnostic(DiagnosticMotionInvalid, path+".motion", "summon spawns do not support motion")
		}
		if spawn.durationTicks != 0 {
			context.addDiagnostic(DiagnosticMotionInvalid, path+".duration_ticks", "summon spawns live for the spawn effect's duration_ticks; remove the spawn's own duration_ticks")
		}
		if spawn.area != nil || spawn.intervalTicks != 0 || spawn.emitLeaveOnStop {
			context.addDiagnostic(DiagnosticMotionInvalid, path+".area", "area membership fields require an area spawn")
		}
		return
	}
	if spawn.kind == "area" {
		validateAreaSpawn(context, spawn)
	} else if spawn.area != nil || spawn.intervalTicks != 0 || spawn.emitLeaveOnStop {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".area", "area membership fields require an area spawn")
	}
	if spawn.durationTicks <= 0 || spawn.durationTicks > context.environment.Limits.MaxLifetimeTicks {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".duration_ticks", "moving spawn duration must be positive and bounded")
	}
	if spawn.motion == nil {
		if spawn.kind == "area" {
			return
		}
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion", "moving spawns require an explicit typed motion definition")
		return
	}
	motion, ok := spawn.motion.(*canonicalMotionIR)
	if !ok {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion", "canonical motion definition is required")
		return
	}
	if !motionSlotEnabled(context.environment.Motion, "frame") {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.frame", "frame is disabled by the motion catalog")
	}
	if !motionSlotEnabled(context.environment.Motion, "completion") {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.completion", "completion is disabled by the motion catalog")
	}
	if !motionPairAllowed(context.environment.Motion, spawn.kind, motion.trajectory.name()) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.trajectory", "spawn and trajectory pair is not allowed by the motion catalog")
	}
	validateMotionStageVariants(context, spawn.kind, motion, path+".motion")
	if spawn.kind == "beam" && motion.trajectory.name() != "stationary" && !motionPairAllowed(context.environment.Motion, "beam", motion.trajectory.name()) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.trajectory", "beam requires stationary motion unless explicitly cataloged")
	}
	if motion.steering != nil && !motionSlotEnabled(context.environment.Motion, "steering") {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.steering", "steering is disabled by the motion catalog")
	}
	if len(motion.offsets) > context.environment.Limits.MaxMotionOffsets {
		context.addDiagnostic(DiagnosticBudgetExceeded, path+".motion.offsets", "motion offsets exceed the environment maximum")
	}
	if len(motion.offsets) > 0 && !motionSlotEnabled(context.environment.Motion, "offsets") {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.offsets", "offsets are disabled by the motion catalog")
	}
	if motion.collision != nil {
		validateMotionCollision(context, motion.collision, path+".motion.collision")
	}
	if motion.carry != nil {
		if !motionSlotEnabled(context.environment.Motion, "carry") || !motionHostFeatureEnabled(context.environment.Motion, "carry") {
			context.addDiagnostic(DiagnosticMotionInvalid, path+".motion.carry", "carry requires enabled motion slot and host feature")
		}
	}
	validateMotionLiterals(context, motion, path+".motion")
}

func validateAreaSpawn(context *compileContext, spawn *spawnIR) {
	path := spawn.source.Path
	if spawn.area == nil {
		context.addDiagnostic(DiagnosticShapeInvalid, path+".area", "area spawn requires a select plan")
		return
	}
	if spawn.intervalTicks <= 0 || spawn.intervalTicks > spawn.durationTicks {
		context.addDiagnostic(DiagnosticShapeInvalid, path+".interval_ticks", "area interval must be positive and no longer than duration")
	}
	if spawn.area.elementType != selectionEntity || spawn.area.limit <= 0 {
		context.addDiagnostic(DiagnosticShapeInvalid, path+".area", "area spawn requires a bounded entity select")
	}
	if spawn.area.limit > context.environment.Limits.MaxAreaMembers {
		context.addDiagnostic(DiagnosticBudgetExceeded, path+".area.limit", "area members exceed the environment maximum")
	}
	if spawn.area.order != nil && spawn.area.order.by == "random" {
		context.addDiagnostic(DiagnosticShapeInvalid, path+".area.order", "area membership order cannot be random")
	}
}

func validateMotionStageVariants(context *compileContext, spawn string, motion *canonicalMotionIR, path string) {
	trajectory := motion.trajectory.name()
	if !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "frame", motionFrameVariant(motion.frame)) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".frame", "frame variant is not allowed by the spawn/trajectory motion catalog")
	}
	if motion.steering != nil && !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "steering", motionSteeringVariant(motion.steering)) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".steering", "steering variant is not allowed by the spawn/trajectory motion catalog")
	}
	for index, offset := range motion.offsets {
		if !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "offset", motionOffsetVariant(offset)) {
			context.addDiagnostic(DiagnosticMotionInvalid, fmt.Sprintf("%s.offsets[%d]", path, index), "offset variant is not allowed by the spawn/trajectory motion catalog")
		}
	}
	if motion.collision != nil && !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "collision", motion.collision.response) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".collision", "collision response is not allowed by the spawn/trajectory motion catalog")
	}
	if motion.carry != nil && !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "carry", "carry") {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".carry", "carry is not allowed by the spawn/trajectory motion catalog")
	}
	if !motionVariantAllowed(context.environment.Motion, spawn, trajectory, "completion", motionCompletionVariant(motion.completion)) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".completion", "completion variant is not allowed by the spawn/trajectory motion catalog")
	}
}

func motionFrameVariant(value motionFrameIR) string {
	switch value.(type) {
	case worldFrameIR:
		return "world"
	case followFrameIR:
		return "follow"
	default:
		return ""
	}
}

func motionSteeringVariant(value motionSteeringIR) string {
	switch value.(type) {
	case trackingSteeringIR:
		return "tracking"
	default:
		return ""
	}
}

func motionOffsetVariant(value motionOffsetIR) string {
	switch value.(type) {
	case zigzagOffsetIR:
		return "zigzag"
	case circularOffsetIR:
		return "circular"
	default:
		return ""
	}
}

func motionCompletionVariant(value motionCompletionIR) string {
	switch value.(type) {
	case endCompletionIR:
		return "end"
	case pauseThenEndCompletionIR:
		return "pause_then_end"
	case boomerangCompletionIR:
		return "boomerang"
	default:
		return ""
	}
}

func validateMotionCollision(context *compileContext, collision *motionCollisionIR, path string) {
	if !motionSlotEnabled(context.environment.Motion, "collision") {
		context.addDiagnostic(DiagnosticMotionInvalid, path, "collision is disabled by the motion catalog")
	}
	if len(collision.layers) == 0 || !uniqueNonEmptyStrings(collision.layers) {
		context.addDiagnostic(DiagnosticMotionInvalid, path+".layers", "collision layers must be a non-empty unique list")
	}
	for index, layer := range collision.layers {
		if _, found := context.artifacts.authority.collision[layer]; !found {
			context.addDiagnostic(DiagnosticCapabilityUnknown, fmt.Sprintf("%s.layers[%d]", path, index), "unknown collision layer")
		}
	}
	switch collision.response {
	case "stop":
		if collision.maxReflects != 0 || collision.maxPierces != 0 {
			context.addDiagnostic(DiagnosticMotionInvalid, path, "stop collision cannot carry reflect or pierce counts")
		}
	case "reflect":
		if collision.maxReflects <= 0 || collision.maxReflects > context.environment.Limits.MaxReflects || collision.maxPierces != 0 {
			context.addDiagnostic(DiagnosticMotionInvalid, path, "reflect requires a bounded positive max_reflects and no max_pierces")
		}
	case "pierce":
		if collision.maxPierces <= 0 || collision.maxPierces > context.environment.Limits.MaxPierces || collision.maxReflects != 0 {
			context.addDiagnostic(DiagnosticMotionInvalid, path, "pierce requires a bounded positive max_pierces and no max_reflects")
		}
	default:
		context.addDiagnostic(DiagnosticMotionInvalid, path+".response", "collision response must be stop, reflect, or pierce")
	}
}

func validateMotionLiterals(context *compileContext, motion *canonicalMotionIR, path string) {
	positive := func(value valueIR, valuePath string, maximum int64) {
		literal, ok := value.(*intValueIR)
		if !ok || literal.value <= 0 || literal.value > maximum {
			context.addDiagnostic(DiagnosticMotionInvalid, valuePath, "motion value must be a positive bounded integer literal")
		}
	}
	positiveTicks := func(value Tick, valuePath string, maximum Tick) {
		if value <= 0 || value > maximum {
			context.addDiagnostic(DiagnosticMotionInvalid, valuePath, "motion duration must be positive and bounded")
		}
	}
	limits, catalog := context.environment.Limits, context.environment.Motion
	switch trajectory := motion.trajectory.(type) {
	case linearTrajectoryIR:
		positive(trajectory.speed, path+".trajectory.speed", catalog.MaximumSpeed)
	case pathTrajectoryIR:
		positive(trajectory.speed, path+".trajectory.speed", catalog.MaximumSpeed)
	case orbitTrajectoryIR:
		positive(trajectory.radius, path+".trajectory.radius", catalog.MaximumDistance)
		positive(trajectory.angularSpeed, path+".trajectory.angular_speed", catalog.MaximumAngularSpeed)
	case parabolaTrajectoryIR:
		positive(trajectory.height, path+".trajectory.height", catalog.MaximumDistance)
		positiveTicks(trajectory.durationTicks, path+".trajectory.duration_ticks", limits.MaxLifetimeTicks)
	}
	if steering, ok := motion.steering.(trackingSteeringIR); ok {
		positiveTicks(steering.durationTicks, path+".steering.duration_ticks", catalog.MaximumTrackingTicks)
	}
	for index, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagOffsetIR:
			positive(typed.amplitude, fmt.Sprintf("%s.offsets[%d].amplitude", path, index), catalog.MaximumDistance)
			positiveTicks(typed.periodTicks, fmt.Sprintf("%s.offsets[%d].period_ticks", path, index), limits.MaxLifetimeTicks)
		case circularOffsetIR:
			positive(typed.radius, fmt.Sprintf("%s.offsets[%d].radius", path, index), catalog.MaximumDistance)
			positive(typed.angularSpeed, fmt.Sprintf("%s.offsets[%d].angular_speed", path, index), catalog.MaximumAngularSpeed)
		}
	}
	switch completion := motion.completion.(type) {
	case pauseThenEndCompletionIR:
		positiveTicks(completion.pauseTicks, path+".completion.pause_ticks", limits.MaxLifetimeTicks)
	case boomerangCompletionIR:
		positiveTicks(completion.maxReturnTicks, path+".completion.max_return_ticks", limits.MaxLifetimeTicks)
	}
}

func validMotionSpawnKind(kind string) bool {
	switch kind {
	case "dash", "orbit", "projectile", "area", "beam", "summon":
		return true
	default:
		return false
	}
}
func motionPairAllowed(catalog MotionCapabilityCatalog, spawn, trajectory string) bool {
	for _, pair := range catalog.SpawnTrajectoryPairs {
		if pair.Spawn == spawn && pair.Trajectory == trajectory {
			return true
		}
	}
	return false
}

func motionVariantAllowed(catalog MotionCapabilityCatalog, spawn, trajectory, stage, variant string) bool {
	for _, capability := range catalog.VariantCapabilities {
		if capability.Spawn != spawn || capability.Trajectory != trajectory {
			continue
		}
		switch stage {
		case "frame":
			return containsString(capability.Frames, variant)
		case "steering":
			return containsString(capability.Steering, variant)
		case "offset":
			return containsString(capability.Offsets, variant)
		case "collision":
			return containsString(capability.CollisionResponses, variant)
		case "carry":
			return capability.Carry && variant == "carry"
		case "completion":
			return containsString(capability.Completions, variant)
		}
	}
	return false
}
func motionSlotEnabled(catalog MotionCapabilityCatalog, slot string) bool {
	for _, value := range catalog.EnabledSlots {
		if value == slot {
			return true
		}
	}
	return false
}
func motionHostFeatureEnabled(catalog MotionCapabilityCatalog, feature string) bool {
	for _, value := range catalog.HostFeatures {
		if value == feature {
			return true
		}
	}
	return false
}
