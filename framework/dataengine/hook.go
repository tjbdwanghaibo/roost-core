package dataengine

// DirtyHook propagates a nested value mutation to its generated parent DAO.
// The callback records persistence in the active Nest transaction and/or marks
// sync state according to the parent field's generated policy.
type DirtyHook struct {
	notify func()
	change func(SyncPath)
}

func (hook *DirtyHook) SetNotify(notify func()) { hook.notify = notify; hook.change = nil }
func (hook *DirtyHook) SetChangeNotify(notify func(SyncPath)) {
	hook.change = notify
	hook.notify = nil
}

func (hook *DirtyHook) Mark() {
	if hook != nil && hook.change != nil {
		hook.change(nil)
		return
	}
	if hook != nil && hook.notify != nil {
		hook.notify()
	}
}

// MarkPath 沿所有权链传递变化位置；普通手写回调仍可只接收标脏通知。
func (hook *DirtyHook) MarkPath(path SyncPath) {
	if hook != nil && hook.change != nil {
		hook.change(path)
		return
	}
	hook.Mark()
}
