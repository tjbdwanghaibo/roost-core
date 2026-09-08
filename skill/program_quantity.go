package skill

type quantityProgram struct {
	path    string
	typ     valueType
	minimum int64
	maximum int64
	proved  bool
}

type QuantityView struct {
	Path     string
	Kind     string
	Quantity string
	Minimum  int64
	Maximum  int64
	Proved   bool
}

func valueKindName(kind valueKind) string {
	names := [...]string{"invalid", "null", "int", "bool", "string", "entity", "position", "hit", "path", "direction", "process", "attribute", "element", "gameplay_tag", "ability", "status_instance", "snapshot_token", "entity_list", "string_list", "effect_result"}
	if int(kind) < len(names) {
		return names[kind]
	}
	return "invalid"
}

func quantityKindName(kind quantityKind) string {
	names := [...]string{"unknown", "dimensionless", "count", "ticks", "world_distance", "angle_mdeg", "speed_world_per_tick", "angular_speed_mdeg_per_tick", "basis_points", "combat_amount", "resource_amount"}
	if int(kind) < len(names) {
		return names[kind]
	}
	return "unknown"
}
