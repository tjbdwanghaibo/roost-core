package def

//roost:dao coll=remote_balances db=roost_remote_generated_placeholder dbscope=global
type BalanceDao struct {
	Value int64 `dao:"persist,sync"`
}

//roost:dao coll=remote_items db=roost_remote_generated_placeholder dbscope=global
type ItemsDao struct {
	Value int64 `dao:"persist,sync"`
}

// GuildDao 是 Mirror 第 5 步公会摘要样例的 owner 状态（mirror_test.go）：Managed Guild 提交它，只读服务
// 经生成的 GuildSummary DTO 读其中两个字段。
//
//roost:dao coll=remote_guilds db=roost_remote_generated_placeholder dbscope=global
type GuildDao struct {
	Name    string `dao:"persist"`
	Members int64  `dao:"persist"`
	Motto   string `dao:"persist"`
}
