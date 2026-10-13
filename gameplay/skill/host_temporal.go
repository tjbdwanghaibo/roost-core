package skill

// TemporalCaptureCommand 请求 Host 按 profile 捕获目标的时间快照。
type TemporalCaptureCommand struct {
	Owner          EntityID
	Target         EntityID
	ProgramID      string
	GameplayDigest string
	Profile        TemporalProfileHandle
	Context        EventContext
}

// TemporalRestoreCommand 请求 Host 用快照 token 恢复目标；OnBlocked 决定位置被阻挡时的处理。
type TemporalRestoreCommand struct {
	Owner          EntityID
	Target         EntityID
	ProgramID      string
	GameplayDigest string
	Token          SnapshotToken
	OnBlocked      string
	Context        EventContext
}

func (TemporalCaptureCommand) isEffectCommandPayload() {}
func (TemporalRestoreCommand) isEffectCommandPayload() {}
