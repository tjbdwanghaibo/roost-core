//go:build terrainexp

package def

// TerrainBytesDao 是方案 A：高度按 2 字节/点小端打包进 []byte，同步与持久化都是整字段替换。
//
//roost:dao coll=terrain_bytes db=terrain
type TerrainBytesDao struct {
	BaseRevision int64  `store:"1"`
	Version      int64  `store:"2"`
	Heights      []byte `store:"3"`
	Materials    []byte `store:"4"`
}

// TerrainListDao 是方案 B：高度为 []uint16 列表，同步走 list delta，持久化按下标路径。
//
//roost:dao coll=terrain_list db=terrain
type TerrainListDao struct {
	BaseRevision int64    `store:"1"`
	Version      int64    `store:"2"`
	Heights      []uint16 `store:"3"`
	Materials    []byte   `store:"4"`
}
