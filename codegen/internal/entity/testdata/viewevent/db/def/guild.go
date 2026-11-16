package dbdef

type NoteBook struct {
	Lines []string `store:"1"`
}

//roost:dao coll=guild db=main dbscope=global
type GuildDao struct {
	ActivityGuild ActivityBook `store:"activity" dao:"persist,nosync"`
	// 两个 NoteBook：view 的 Notes() 按名字对不上、按类型有歧义，交给手写。
	Board   NoteBook `store:"board" dao:"persist,nosync"`
	Archive NoteBook `store:"archive" dao:"persist,nosync"`
}
