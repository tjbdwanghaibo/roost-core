package skill

type spawnTemplateProgram struct {
	index           SpawnTemplateIndex
	durationTicks   Tick
	intervalTicks   Tick
	emitLeaveOnStop bool
	visual          VisualIndex
	hasVisual       bool
	area            *selectorProgram
	motion          *motionProgram
	numericTracks   []numericTrackProgram
	callbacks       []spawnCallbackProgram
}

type spawnNumericOperation uint8

const (
	spawnNumericSet spawnNumericOperation = iota + 1
	spawnNumericAdd
	spawnNumericMulBP
)

type spawnNumericInterpolation uint8

const spawnNumericLinearInteger spawnNumericInterpolation = 1

type spawnNumericRounding uint8

const spawnNumericTruncateTowardZero spawnNumericRounding = 1

type spawnPropertyKey uint8

const (
	spawnPropertySpeed spawnPropertyKey = iota + 1
	spawnPropertyRadius
	spawnPropertyArcHeight
	spawnPropertyTurnRateMDegPerTick
	spawnPropertyAngularSpeedMDegPerTick
	spawnPropertyOffsetAmplitude
	spawnPropertyOffsetRadius
	spawnPropertyReturnSpeedBP
	spawnPropertyCollisionForce
)

type spawnPropertySpawnKind uint8

const (
	spawnPropertySpawnDash spawnPropertySpawnKind = iota + 1
	spawnPropertySpawnOrbit
	spawnPropertySpawnProjectile
	spawnPropertySpawnArea
)

type spawnPropertySlotStage uint8

const (
	spawnPropertySlotTrajectory spawnPropertySlotStage = iota + 1
	spawnPropertySlotSteering
	spawnPropertySlotOffset
	spawnPropertySlotCompletion
	spawnPropertySlotCollision
)

type spawnPropertySlotVariant uint8

const (
	spawnPropertyVariantLinear spawnPropertySlotVariant = iota + 1
	spawnPropertyVariantPath
	spawnPropertyVariantParabola
	spawnPropertyVariantOrbit
	spawnPropertyVariantTracking
	spawnPropertyVariantZigzag
	spawnPropertyVariantCircular
	spawnPropertyVariantBoomerang
	spawnPropertyVariantPresent
)

type spawnPropertySlotField uint8

const (
	spawnPropertyFieldSpeed spawnPropertySlotField = iota + 1
	spawnPropertyFieldRadius
	spawnPropertyFieldHeight
	spawnPropertyFieldTurnRateMDegPerTick
	spawnPropertyFieldAngularSpeed
	spawnPropertyFieldAmplitude
	spawnPropertyFieldReturnSpeedBP
	spawnPropertyFieldForce
)

type spawnPropertySlotBindingProgram struct {
	stage   spawnPropertySlotStage
	variant spawnPropertySlotVariant
	field   spawnPropertySlotField
}

type spawnPropertyProgram struct {
	handle                SpawnPropertyHandle
	key                   spawnPropertyKey
	minimum, maximum      int64
	interpolation         spawnNumericInterpolation
	rounding              spawnNumericRounding
	allowedOperationsMask uint8
	spawnKinds            []spawnPropertySpawnKind
	slotBindings          []spawnPropertySlotBindingProgram
}

type numericTrackProgram struct {
	property  SpawnPropertyHandle
	operation spawnNumericOperation
	value     programValue
	overTicks Tick
}

type spawnCallbackProgram struct {
	event     string
	operation OperationIndex
}

// motionProgram is immutable after lowering. Every field contains a concrete
// runtime payload; source Wire/IR values and catalog string keys never escape
// the compiler.
type motionProgram struct {
	frame      motionFrameProgram
	steering   motionSteeringProgram
	trajectory motionTrajectoryProgram
	offsets    []motionOffsetProgram
	collision  *motionCollisionProgram
	carry      *motionCarryProgram
	completion motionCompletionProgram
}

type motionFrameProgram interface{ isMotionFrameProgram() }
type worldMotionFrameProgram struct{}
type followMotionFrameProgram struct{ target programValue }

func (worldMotionFrameProgram) isMotionFrameProgram()  {}
func (followMotionFrameProgram) isMotionFrameProgram() {}

type motionSteeringProgram interface{ isMotionSteeringProgram() }
type fixedMotionSteeringProgram struct{}
type trackingMotionSteeringProgram struct {
	target        programValue
	durationTicks Tick
}

func (fixedMotionSteeringProgram) isMotionSteeringProgram()    {}
func (trackingMotionSteeringProgram) isMotionSteeringProgram() {}

type motionTrajectoryProgram interface{ isMotionTrajectoryProgram() }
type stationaryMotionTrajectoryProgram struct{}
type linearMotionTrajectoryProgram struct{ speed programValue }
type pathMotionTrajectoryProgram struct{ points, speed programValue }
type orbitMotionTrajectoryProgram struct{ anchor, radius, angularSpeed programValue }
type parabolaMotionTrajectoryProgram struct {
	destination, height programValue
	durationTicks       Tick
}

func (stationaryMotionTrajectoryProgram) isMotionTrajectoryProgram() {}
func (linearMotionTrajectoryProgram) isMotionTrajectoryProgram()     {}
func (pathMotionTrajectoryProgram) isMotionTrajectoryProgram()       {}
func (orbitMotionTrajectoryProgram) isMotionTrajectoryProgram()      {}
func (parabolaMotionTrajectoryProgram) isMotionTrajectoryProgram()   {}

type motionOffsetProgram interface{ isMotionOffsetProgram() }
type zigzagMotionOffsetProgram struct {
	amplitude   programValue
	periodTicks Tick
}
type circularMotionOffsetProgram struct{ radius, angularSpeed programValue }

func (zigzagMotionOffsetProgram) isMotionOffsetProgram()   {}
func (circularMotionOffsetProgram) isMotionOffsetProgram() {}

type motionCollisionResponse uint8

const (
	motionCollisionStop motionCollisionResponse = iota + 1
	motionCollisionReflect
	motionCollisionPierce
)

type motionCollisionProgram struct {
	layers                  []CollisionLayerHandle
	response                motionCollisionResponse
	maxReflects, maxPierces int
}

type motionCarryProgram struct{ target programValue }

type motionCompletionProgram interface{ isMotionCompletionProgram() }
type endMotionCompletionProgram struct{}
type pauseThenEndMotionCompletionProgram struct{ pauseTicks Tick }
type boomerangMotionCompletionProgram struct{ maxReturnTicks Tick }

func (endMotionCompletionProgram) isMotionCompletionProgram()          {}
func (pauseThenEndMotionCompletionProgram) isMotionCompletionProgram() {}
func (boomerangMotionCompletionProgram) isMotionCompletionProgram()    {}

type SpawnTemplateView struct {
	Index     SpawnTemplateIndex
	Callbacks []OperationIndex
}
