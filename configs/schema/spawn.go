package schema

// Spawn is one line of the scene's population: how many of a monster to keep
// alive, where, and how long after one dies before it comes back.
//
// It is config data rather than code for the same reason the item table is:
// a designer changing "three wolves, respawn in twenty seconds" into "five
// wolves, respawn in ten" is a table edit and a hot reload, not a build. The
// spawner reads the active snapshot, so a reload takes effect on the next
// tick without restarting the scene.
//
//roost:table name=spawn key=ID
type Spawn struct {
	ID       int32 `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
	Template int32 `csv:"template" json:"template" title:"Template" required:"true"`
	// Count is how many of this row should be alive at once.
	Count int32 `csv:"count" json:"count" title:"Count" required:"true"`
	// HP is what one of them starts with.
	HP int64 `csv:"hp" json:"hp" title:"HP" required:"true"`
	// CenterX / CenterY / Radius is where they appear: a point and how far
	// around it the spawner may look for somewhere to stand.
	CenterX int64 `csv:"center_x" json:"center_x" title:"CenterX" required:"true"`
	CenterY int64 `csv:"center_y" json:"center_y" title:"CenterY" required:"true"`
	Radius  int64 `csv:"radius" json:"radius" title:"Radius" required:"true"`
	// RespawnSeconds is how long after a death before that slot is refilled.
	RespawnSeconds int64 `csv:"respawn_seconds" json:"respawn_seconds" title:"RespawnSeconds" required:"true"`
}
