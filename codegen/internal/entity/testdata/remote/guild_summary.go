package remote

// GuildSummary is a read-only view of the Guild owner's "guilds" DAO: another
// service reads it through the generated NewGuildSummaryReader and never holds
// the guild itself (Mirror step 5).
//
//roost:mirror entityKind=EntityKindGuild coll=guilds
type GuildSummary struct {
	Name string `bson:"name"`
}
