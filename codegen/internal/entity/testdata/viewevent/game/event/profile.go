package event

// ProfileLevelUp 在 Player 升级成功后、同一笔事务内发布。
type ProfileLevelUp struct{ From, To int32 }

// TaskCompleted 在任务完成时发布。
type TaskCompleted struct{ ID int64 }

// ProfileRenaming 在改名之前发布（拦截类）。
type ProfileRenaming struct{ Name string }

// notAnEvent 不导出，不算事件。
type notAnEvent struct{}
