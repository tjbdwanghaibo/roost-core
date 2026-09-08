package skill

type CastWindowDefinition struct {
	WindupTicks Tick `json:"windup_ticks"`
	// WindupTicksExpression, when set, replaces WindupTicks with a value
	// evaluated as the cast window is prepared. The result is clamped into
	// [WindupTicksMin, WindupTicksMax], so the compile-time bounds stay the
	// worst case no matter what the expression yields; CommitTick must fit
	// under WindupTicksMin. RecoveryTicksExpression works the same way and is
	// evaluated when recovery begins.
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
