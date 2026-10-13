package nestwal

// EffectEnvelope 是 DataEngine outbox 发布到 JetStream 的 effect 消息体（JSON），
// 由 dataengine/engine 的 outbox 发布器编码；消费方按 EffectID 去重。
type EffectEnvelope struct {
	TransactionID string            `json:"transaction_id"`
	EffectID      string            `json:"effect_id"`
	Topic         string            `json:"topic"`
	Key           string            `json:"key,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Payload       []byte            `json:"payload"`
}
