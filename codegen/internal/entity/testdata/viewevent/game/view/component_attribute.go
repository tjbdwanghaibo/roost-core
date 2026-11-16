package view

import "example.com/g/db"

type Attribute interface{ Refresh() error }

type AttributeHolder interface {
	Attribute() Attribute
	AttributeBase() *db.AttributeValues
	AttributeGear() *db.AttributeValues
	AttributeFinal() *db.AttributeValues
	// AttributeLevel 是基础类型：只认完全同名（PlayerDao.Level 不算，去组件前缀只对结构体生效），手写。
	AttributeLevel() int32
}
