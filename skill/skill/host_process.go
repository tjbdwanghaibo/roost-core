package skill

type ProcessCommandMeta struct {
	RequiredRevision WorldRevision
	ProcessID        ProcessID
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
type SignalsMotionStep struct{ Signals []ProcessSignal }

func (FrameMotionStep) isMotionStep()      {}
func (SteeringMotionStep) isMotionStep()   {}
func (TrajectoryMotionStep) isMotionStep() {}
func (OffsetsMotionStep) isMotionStep()    {}
func (CollisionMotionStep) isMotionStep()  {}
func (CarryMotionStep) isMotionStep()      {}
func (CompletionMotionStep) isMotionStep() {}
func (SignalsMotionStep) isMotionStep()    {}

type ProcessNumericSnapshot struct {
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

type ProcessStepCommand struct {
	Meta    ProcessCommandMeta
	Motion  MotionStep
	Numeric ProcessNumericSnapshot
}

type ProcessStopCommand struct{ Meta ProcessCommandMeta }

type ProcessHostState struct {
	ProcessID ProcessID
	Position  Position
	Active    bool
}

type ProcessStepResult struct {
	Commit  CommitReceipt
	State   ProcessHostState
	Signals []ProcessSignal
}
