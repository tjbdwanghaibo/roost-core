package view

import "example.com/g/db"

type Bag interface{ Count(itemID int64) int64 }

// BagHolder：访问器 Bag() 之外的 BagState 是宿主方法（按类型交给 PlayerDao.Bag）。
type BagHolder interface {
	Bag() Bag
	BagState() *db.BagState
}
