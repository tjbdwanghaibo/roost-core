package skill

type CastWindowDefinition struct {
	WindupTicks Tick `json:"windup_ticks"`
	// WindupTicksExpression 设置时取代 WindupTicks，在准备施法窗口时求值。结果夹在 [WindupTicksMin, WindupTicksMax]
	// 内，无论表达式算出什么，编译期上下界都是最坏情况；CommitTick 不得超过 WindupTicksMin。
	// RecoveryTicksExpression 同理，在后摇开始时求值。
	WindupTicksExpression   Value    `json:"-"`
	WindupTicksMin          Tick     `json:"windup_ticks_min"`
	WindupTicksMax          Tick     `json:"windup_ticks_max"`
	CommitTick              Tick     `json:"commit_tick"`
	RecoveryTicks           Tick     `json:"recovery_ticks"`
	RecoveryTicksExpression Value    `json:"-"`
	RecoveryTicksMin        Tick     `json:"recovery_ticks_min"`
	RecoveryTicksMax        Tick     `json:"recovery_ticks_max"`
	Movement                string   `json:"movement"`
	Turning                 string   `json:"turning"`
	InterruptTags           []string `json:"interrupt_tags"`
	RefundBeforeCommit      bool     `json:"refund_before_commit"`
}
