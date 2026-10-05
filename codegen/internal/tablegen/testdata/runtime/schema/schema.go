// Package schema is the tablegen runtime gate's meta: a scene table and a
// monster table whose scene_id refers to it (RR-20261005-NC-75).
package schema

//roost:table name=scene key=ID
type Scene struct {
	ID   int32  `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
	Name string `csv:"name" json:"name" title:"Name" required:"true"`
}

//roost:table name=monster key=ID
type Monster struct {
	ID      int32 `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
	SceneID int32 `csv:"scene_id" json:"scene_id" title:"SceneID" ref:"scene"`
	Level   int32 `csv:"level" json:"level" title:"Level" required:"true" min:"1"`
}
