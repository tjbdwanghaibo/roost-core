package skill

type processTemplateProgram struct {
	index           ProcessTemplateIndex
	durationTicks   Tick
	intervalTicks   Tick
	emitLeaveOnStop bool
	visual          VisualIndex
	hasVisual       bool
	area            *selectorProgram
	motion          *motionProgram
	numericTracks   []numericTrackProgram
	callbacks       []processCallbackProgram
}

type processNumericOperation uint8

const (
	processNumericSet processNumericOperation = iota + 1
	processNumericAdd
	processNumericMulBP
)

type processNumericInterpolation uint8

const processNumericLinearInteger processNumericInterpolation = 1

type processNumericRounding uint8

const processNumericTruncateTowardZero processNumericRounding = 1

type processPropertyKey uint8

const (
	processPropertySpeed processPropertyKey = iota + 1
	processPropertyRadius
	processPropertyArcHeight
	processPropertyTurnRateMDegPerTick
	processPropertyAngularSpeedMDegPerTick
	processPropertyOffsetAmplitude
	processPropertyOffsetRadius
	processPropertyReturnSpeedBP
	processPropertyCollisionForce
)

type processPropertyProcessKind uint8

const (
	processPropertyProcessDash processPropertyProcessKind = iota + 1
	processPropertyProcessOrbit
	processPropertyProcessProjectile
	processPropertyProcessArea
)

type processPropertySlotStage uint8

const (
	processPropertySlotTrajectory processPropertySlotStage = iota + 1
	processPropertySlotSteering
	processPropertySlotOffset
	processPropertySlotCompletion
	processPropertySlotCollision
)

type processPropertySlotVariant uint8

const (
	processPropertyVariantLinear processPropertySlotVariant = iota + 1
	processPropertyVariantPath
	processPropertyVariantParabola
	processPropertyVariantOrbit
	processPropertyVariantTracking
	processPropertyVariantZigzag
	processPropertyVariantCircular
	processPropertyVariantBoomerang
	processPropertyVariantPresent
)

type processPropertySlotField uint8

const (
	processPropertyFieldSpeed processPropertySlotField = iota + 1
	processPropertyFieldRadius
	processPropertyFieldHeight
	processPropertyFieldTurnRateMDegPerTick
	processPropertyFieldAngularSpeed
	processPropertyFieldAmplitude
	processPropertyFieldReturnSpeedBP
	processPropertyFieldForce
)

type processPropertySlotBindingProgram struct {
	stage   processPropertySlotStage
	variant processPropertySlotVariant
	field   processPropertySlotField
}

type processPropertyProgram struct {
	handle                ProcessPropertyHandle
	key                   processPropertyKey
	minimum, maximum      int64
	interpolation         processNumericInterpolation
	rounding              processNumericRounding
	allowedOperationsMask uint8
	processKinds          []processPropertyProcessKind
	slotBindings          []processPropertySlotBindingProgram
}

type numericTrackProgram struct {
	property  ProcessPropertyHandle
	operation processNumericOperation
	value     programValue
	overTicks Tick
}

type processCallbackProgram struct {
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

type ProcessTemplateView struct {
	Index     ProcessTemplateIndex
	Callbacks []OperationIndex
}
