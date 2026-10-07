package mods

// SagaPayloadConfig 是 saga.max_payload_bytes：saga 启动意图与步骤结果 Data 的上限（RR-20261006-66，F06-S7 跨进程）。
// 发起方（kit/nest：每个调 EmitStart 的进程都有 Nest）与协调器（kit/saga）的 Mod 都匿名嵌入它，两边共用同一份声明
// （A4 ① 的 Merge 要求同一键的声明完全相同）。发起方按它在 Nest 事务里拒绝超限的启动意图，协调器按它作为
// saga.Options.MaxPayloadBytes；两类进程的配置要写同一个值。不写进生成的配置段：没有 saga 的工程不该多出 saga 段。
type SagaPayloadConfig struct {
	SagaMaxPayloadBytes int `config:"saga.max_payload_bytes" default:"65536" min:"1" max:"4194304" help:"Upper bound of a saga start intent's and step result's Data. Shared by the\ninitiators (EmitStart refuses over it inside the Nest transaction) and the\ncoordinator; write the same value in both."`
}

// SagaPayloadLimit 返回 saga.max_payload_bytes。两个 Mod 都经它读，声明与读取在同一个包（声明守卫按包检查读取）。
func (c SagaPayloadConfig) SagaPayloadLimit() int { return c.SagaMaxPayloadBytes }
