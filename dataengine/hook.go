package dataengine

// DirtyHook propagates a nested value mutation to its generated parent DAO.
// The callback records persistence in the active Nest transaction and/or marks
// sync state according to the parent field's generated policy.
type DirtyHook struct {
	notify func()
}

func (hook *DirtyHook) SetNotify(notify func()) { hook.notify = notify }

func (hook *DirtyHook) Mark() {
	if hook != nil && hook.notify != nil {
		hook.notify()
	}
}
