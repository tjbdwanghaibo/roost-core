package player

import db "example.com/g/db"

// 手写的宿主方法：每种实体自己的值、有参数的业务钩子、DAO 里有歧义的字段。
func (e *Player) ActivityScope() string                        { return "player" }
func (e *Player) Notes() *db.NoteBook                          { return nil }
func (e *Player) WalletSpent(amount, nowUnixMilli int64) error { return nil }
