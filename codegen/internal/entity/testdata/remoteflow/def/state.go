package def

//roost:dao coll=remote_balances db=roost_remote_generated_placeholder dbscope=global
type BalanceDao struct {
	Value int64 `dao:"persist,sync" store:"value"`
}

//roost:dao coll=remote_items db=roost_remote_generated_placeholder dbscope=global
type ItemsDao struct {
	Value int64 `dao:"persist,sync" store:"value"`
}

// GuildDao 是 Mirror 第 5 步公会摘要样例的 owner 状态（mirror_test.go）：Managed Guild 提交它，只读服务
// 经生成的 GuildSummary DTO 读其中两个字段。
//
//roost:dao coll=remote_guilds db=roost_remote_generated_placeholder dbscope=global
type GuildDao struct {
	Name    string `dao:"nosync" store:"name"`
	Members int64  `dao:"nosync" store:"members"`
	Motto   string `dao:"nosync" store:"motto"`
}
