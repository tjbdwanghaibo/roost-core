package remoteflow

// GuildSummary 是只读服务看到的公会摘要（Mirror 第 5 步）：声明要读的字段即可，生成器给出
// GuildSummaryMirrorSpec / DecodeGuildSummary / NewGuildSummaryReader；owner DAO 的其他字段（Motto）不解码。
//
//roost:mirror entityKind=EntityKindGuild coll=remote_guilds
type GuildSummary struct {
	Name    string `bson:"name"`
	Members int64  `bson:"members"`
}
