package saga

// 守卫的负对照（step_transition_guard_test.go 解析本目录，不参与编译）：两种不手拼 ApplyRequest 字面量、
// 也不改 Incarnation，却绕过 stepTransition 决定的写法。

import "context"

// 先经 stepTransition 再改它的结果：关闭哪个操作被改掉了。
func (e *Engine) mutatedTransitionExit(ctx context.Context, record Record) error {
	request := stepTransition(record, record, transition{cause: causeTimeout, fenced: true})
	request.CloseOperation = ""
	_, err := e.store.Apply(ctx, request)
	return err
}

// 不写字面量地逐字段拼请求，再经 store 的别名写入。
func (e *Engine) aliasedStoreExit(ctx context.Context, record Record) error {
	var request ApplyRequest
	request.ExpectedVersion = record.Version
	request.After = record
	store := e.store
	_, err := store.Apply(ctx, request)
	return err
}
