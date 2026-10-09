package gateway

import "github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"

// 指标只使用角色/阶段/结果等有限标签，不含玩家、会话、票据或进程 token。
func observeResult(role, phase string, outcome Outcome) {
	result := "unknown"
	if outcome == Completed {
		result = "completed"
	} else if outcome == NotAdmitted {
		result = "not_admitted"
	}
	metrics.IncCounter("gate.channel.result_total", metrics.Labels{"role": role, "phase": phase, "result": result}, 1)
}
func observeFailure(role, phase string) {
	metrics.IncCounter("gate.channel.failure_total", metrics.Labels{"role": role, "phase": phase}, 1)
}

type IngressStats struct {
	Bindings, ForwardRequests int
	ControlRequests           int
	ControlBytes              int64
	ForwardBytes              int64
}

func (game *GameIngress) Stats() IngressStats {
	game.mu.Lock()
	defer game.mu.Unlock()
	return IngressStats{Bindings: len(game.resources), ForwardRequests: game.residentRequests, ForwardBytes: game.residentBytes, ControlRequests: game.controlRequests, ControlBytes: game.controlBytes}
}
