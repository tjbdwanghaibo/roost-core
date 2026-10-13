package skill

// SpawnCommandMeta 是衍生物命令共有的元数据：要求的世界 revision、衍生物 ID 与效果下标。
type SpawnCommandMeta struct {
	RequiredRevision WorldRevision
	SpawnID          SpawnID
	EffectIndex      EffectIndex
}

// MotionStep 是交给 Host 的一个运动阶段步骤。各个变体刻意按阶段区分，只携带已解析的整数运行期事实，Host 不需要
// 查看 Program、IR、Wire 定义或按字符串键的运动策略。
type MotionStep interface{ isMotionStep() }

// StaticMotionStep 等是各运动阶段的步骤：静止、参考系、转向、轨迹、偏移、碰撞、携带、完成与信号。
type StaticMotionStep struct{ Position Position }

func (StaticMotionStep) isMotionStep() {}

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

// SpawnNumericSnapshot 是这一步衍生物数值属性的当前值。
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

// SpawnStepCommand 是 Host.StepSpawn 的请求：一个运动阶段步骤与数值快照。
type SpawnStepCommand struct {
	Meta    SpawnCommandMeta
	Motion  MotionStep
	Numeric SpawnNumericSnapshot
}

// SpawnStopCommand 是 Host.StopSpawn 的请求；Host 必须幂等处理。
type SpawnStopCommand struct{ Meta SpawnCommandMeta }

// SpawnHostState 是 Host 侧衍生物的状态，随 Runtime 的衍生物记录保存。
type SpawnHostState struct {
	SpawnID  SpawnID
	Position Position
	Active   bool
}

// SpawnStepResult 是 StepSpawn 的结果：提交回执、新的 Host 侧状态与产生的信号。
type SpawnStepResult struct {
	Commit  CommitReceipt
	State   SpawnHostState
	Signals []SpawnSignal
}
