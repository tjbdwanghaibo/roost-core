package entitysync

import (
	"errors"
	"maps"
	"slices"
)

type policyHook struct {
	apply   func() error
	pending func() bool
}

// RegisterPolicy 安装锁外、捕获前的兴趣事实处理器。处理器不得调用 Flush，
// 不得读取未冻结的业务字段；可更新订阅。返回取消函数供政策关闭时释放。
func (m *Manager) RegisterPolicy(apply func() error, pending ...func() bool) func() {
	if apply == nil {
		return func() {}
	}
	m.policyMu.Lock()
	if m.policies == nil {
		m.policies = make(map[uint64]policyHook)
	}
	m.nextPolicy++
	id := m.nextPolicy
	hook := policyHook{apply: apply}
	if len(pending) > 0 {
		hook.pending = pending[0]
	}
	m.policies[id] = hook
	m.policyMu.Unlock()
	return func() { m.policyMu.Lock(); delete(m.policies, id); m.policyMu.Unlock() }
}
func (m *Manager) policyHooks() []policyHook {
	m.policyMu.Lock()
	defer m.policyMu.Unlock()
	hooks := make([]policyHook, 0, len(m.policies))
	for _, id := range slices.Sorted(maps.Keys(m.policies)) {
		hooks = append(hooks, m.policies[id])
	}
	return hooks
}
func (m *Manager) applyPolicies() error {
	var err error
	for _, hook := range m.policyHooks() {
		err = errors.Join(err, hook.apply())
	}
	return err
}
func (m *Manager) policiesPending() bool {
	for _, hook := range m.policyHooks() {
		if hook.pending != nil && hook.pending() {
			return true
		}
	}
	return false
}
