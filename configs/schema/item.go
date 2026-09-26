package schema

// Item is the demo's one config table. `roost generate` turns this type into
// a typed loader (configs/generated/gen_table_config.go), converts
// configs/table/item.csv into configs/data/item.json, and registers the table
// with roost-core/configdata; gameplay code then reads it as
// generated.ItemByID(id) off the snapshot pinned to the current request.
//
// The csv tag is the column, title is the human header row, and the rules
// (required / unique) are enforced when the CSV is converted, so bad data
// fails `make generate`, not a player's request.
//
//roost:table name=item key=ID
type Item struct {
	ID       int64  `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
	Name     string `csv:"name" json:"name" title:"Name" required:"true"`
	MaxStack int32  `csv:"max_stack" json:"max_stack" title:"MaxStack" required:"true"`
	// Attack is what holding this item is worth in the attribute system: the
	// player's gear layer is the sum of it over the bag. Config data is
	// where a number like this belongs — changing the sword's attack is a
	// table edit and a hot reload, not a build.
	Attack int64 `csv:"attack" json:"attack" title:"Attack"`
	HP     int64 `csv:"hp" json:"hp" title:"HP"`
}
