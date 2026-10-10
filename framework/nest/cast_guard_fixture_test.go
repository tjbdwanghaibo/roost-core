package nest

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

// 旧动态 Cast 回归继续覆盖 Guard 的锁序、销毁竞态及回滚捕获防御。
// 这些用例刻意制造如今正式准入不可达的争锁交错，因此在测试中补声明后调用真实 Cast。
// 本适配器不代表合法的生产改目标方式、不验证 tail；正式拒绝及共享 tail 由 await_test 覆盖。
// 所有修改只作用于当前测试 goroutine 的 Msg，调用结束恢复，不改变生产准入实现。
func declareGuardFixtureTargets(ids ...int64) func() {
	msg := currentNestDispatchMsg()
	if msg == nil {
		return func() {}
	}
	old := msg.Tids
	msg.Tids = append(append([]int64(nil), old...), ids...)
	return func() { msg.Tids = old }
}
func guardFixtureCastOne[E entity.IThreadSafeEntity](id int64) (E, error) {
	defer declareGuardFixtureTargets(id)()
	return CastOne[E](id)
}
func guardFixtureCastTargetOne[E entity.IThreadSafeEntity](target CastTarget) (E, error) {
	defer declareGuardFixtureTargets(target.ID)()
	return CastTargetOne[E](target)
}
func guardFixtureCastMulti(targets ...CastTarget) ([]entity.IThreadSafeEntity, error) {
	ids := make([]int64, len(targets))
	for i, target := range targets {
		ids[i] = target.ID
	}
	defer declareGuardFixtureTargets(ids...)()
	return CastMulti(targets...)
}
func guardFixtureCastTwo[A, B entity.IThreadSafeEntity](a, b CastTarget) (A, B, error) {
	defer declareGuardFixtureTargets(a.ID, b.ID)()
	return CastTwo[A, B](a, b)
}
func guardFixtureCastThree[A, B, C entity.IThreadSafeEntity](a, b, c CastTarget) (A, B, C, error) {
	defer declareGuardFixtureTargets(a.ID, b.ID, c.ID)()
	return CastThree[A, B, C](a, b, c)
}

// GuardFixtureCastOneForTest 仅导出给 nest_test 的真实 WAL 防御回归，不进入生产构建。
func GuardFixtureCastOneForTest[E entity.IThreadSafeEntity](id int64) (E, error) {
	return guardFixtureCastOne[E](id)
}
