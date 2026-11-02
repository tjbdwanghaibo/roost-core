package def

// 数字存储键与可读字段名使用同一生成链路；业务 API 与 PB 字段名不受 BSON 键影响。
//
//roost:dao coll=short_keys db=game
type ShortKeysDao struct {
	Experience int64                `store:"1"`
	Equipment  map[int64]*ShortItem `store:"2"`
	Followers  []*ShortItem         `store:"3"`
}

type ShortItem struct {
	Strength int32 `store:"1"`
	Quantity int32 `store:"2"`
}

//roost:dao coll=long_keys db=game
type LongKeysDao struct {
	Experience int64               `store:"experience"`
	Equipment  map[int64]*LongItem `store:"equipment"`
	Followers  []*LongItem         `store:"followers"`
}

type LongItem struct {
	Strength int32 `store:"strength"`
	Quantity int32 `store:"quantity"`
}
