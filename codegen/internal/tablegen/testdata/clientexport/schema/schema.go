// Package schema is the client-export fixture (配置表导出给客户端，tablegen 组):
// two ordinary tables (monster carries a server-only field, a client-only
// field and a non-unique index), a layout=kv object and a client-only table.
package schema

//roost:table name=scene key=ID
type Scene struct {
	ID   int32  `csv:"id" json:"id" title:"编号" required:"true"`
	Name string `csv:"name" json:"name" title:"名字" required:"true"`
}

//roost:table name=monster key=ID
type Monster struct {
	ID       int32  `csv:"id" json:"id" title:"编号" required:"true"`
	SceneID  int32  `csv:"scene_id" json:"scene_id" title:"场景" ref:"scene" index:"true"`
	Level    int32  `csv:"level" json:"level" title:"等级" min:"1"`
	DropSeed int64  `csv:"drop_seed" json:"drop_seed" title:"掉落种子" side:"s"`
	Model    string `csv:"model" json:"model" title:"模型" side:"c" required:"true"`
	Tags     string `csv:"tags" json:"tags" title:"标签" parser:"json"`
}

//roost:object name=balance layout=kv
type Balance struct {
	MaxLevel   int32  `csv:"max_level" json:"max_level" title:"最高等级" required:"true" min:"1"`
	StartGold  int64  `csv:"start_gold" json:"start_gold" title:"初始金币"`
	ServerTick int32  `csv:"server_tick" json:"server_tick" title:"服务端心跳" side:"s"`
	ShopBanner string `csv:"shop_banner" json:"shop_banner" title:"商店横幅" side:"c"`
}

//roost:table name=tip side=c key=ID
type Tip struct {
	ID   int32  `csv:"id" json:"id" required:"true"`
	Text string `csv:"text" json:"text"`
}
