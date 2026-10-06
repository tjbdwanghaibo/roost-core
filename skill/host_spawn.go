package skill

type SpawnCommandMeta struct {
	RequiredRevision WorldRevision
	SpawnID          SpawnID
	EffectIndex      EffectIndex
}

type MotionStep interface{ isMotionStep() }

type StaticMotionStep struct{ Position Position }

func (StaticMotionStep) isMotionStep() {}

// MotionStep variants are intentionally stage-specific. They contain only
// resolved integer runtime facts, so a Host never needs to inspect Program,
// IR, Wire definitions, or string-keyed motion policy.
type FrameMotionStep struct{ Position Position }
type SteeringMotionStep struct{ Direction Direction }
type TrajectoryMotionStep struct {
	From, Position Position
	Direction      Direction
}
type OffsetsMotionStep struct{ Position Position }
type CollisionMotionStep struct {
	From, Position Position
	Layers         []CollisionLayerHandle
}
type CarryMotionStep struct {
	Target   EntityID
	Position Position
	Attached bool
}
type CompletionMotionStep struct{ Complete bool }
type SignalsMotionStep struct{ Signals []SpawnSignal }

func (FrameMotionStep) isMotionStep()      {}
func (SteeringMotionStep) isMotionStep()   {}
func (TrajectoryMotionStep) isMotionStep() {}
func (OffsetsMotionStep) isMotionStep()    {}
func (CollisionMotionStep) isMotionStep()  {}
func (CarryMotionStep) isMotionStep()      {}
func (CompletionMotionStep) isMotionStep() {}
func (SignalsMotionStep) isMotionStep()    {}

type SpawnNumericSnapshot struct {
	Speed                   int64
	Radius                  int64
	ArcHeight               int64
	TurnRateMDegPerTick     int64
	AngularSpeedMDegPerTick int64
	OffsetAmplitude         int64
	OffsetRadius            int64
	ReturnSpeedBP           int64
	CollisionForce          int64
}

type SpawnStepCommand struct {
	Meta    SpawnCommandMeta
	Motion  MotionStep
	Numeric SpawnNumericSnapshot
}

type SpawnStopCommand struct{ Meta SpawnCommandMeta }

type SpawnHostState struct {
	SpawnID  SpawnID
	Position Position
	Active   bool
}

type SpawnStepResult struct {
	Commit  CommitReceipt
	State   SpawnHostState
	Signals []SpawnSignal
}
