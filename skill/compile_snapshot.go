package skill

func runSnapshotPass(context *compileContext) {
	context.artifacts.snapshots.reads = make(map[string]AttributeReadPlan)
	attributes := map[string]AttributeCatalogEntry{}
	for _, entry := range context.environment.Gameplay.Attributes.Entries {
		attributes[entry.Key] = entry
	}
	slot := 0
	context.artifacts.ir.walkValues(func(value valueIR) {
		read, ok := value.(*attributeReadValueIR)
		if !ok {
			return
		}
		entry, ok := attributes[read.attribute]
		if !ok {
			context.addDiagnostic(DiagnosticCapabilityUnknown, read.source.Path+".read_attribute.attribute", "unknown attribute")
			return
		}
		point := snapshotPoint(read.snapshot)
		if point == "" {
			point = snapshotCurrent
		}
		if !containsString(entry.Snapshots, string(point)) {
			context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.snapshot", "snapshot point is not allowed for attribute")
			return
		}
		context.artifacts.snapshots.reads[read.source.Path] = AttributeReadPlan{Entity: read.entity, Attribute: entry.Handle, Snapshot: point, SnapshotSlot: slot}
		slot++
	})
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
