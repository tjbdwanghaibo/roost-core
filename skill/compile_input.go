package skill

func runInputStatePass(context *compileContext) {
	input := context.artifacts.ir.input
	clampPolicy := input.clampPolicy
	if clampPolicy == "" {
		clampPolicy = "reject"
	}
	layout := InputLayout{
		Kind: input.kind, Slots: map[string]valueType{}, MaximumPathPoints: input.maximumPoints, MaximumPathLength: input.maximumTotalLength,
		MinimumLength: input.minimumLength, MaximumLength: input.maximumLength, MinimumSegmentLength: input.minimumSegmentLength,
		ClampPolicy: clampPolicy, SimplificationPolicy: input.simplificationPolicy, UpdatePorts: map[InputPort]bool{},
	}
	if input.maximumRange != nil {
		layout.MaximumRange, layout.HasMaximumRange = *input.maximumRange, true
	}
	add := func(name string, kind valueKind) { layout.Slots[name] = valueType{Base: kind} }
	switch input.kind {
	case inputNone:
	case inputDirection:
		add("$input.direction", valueKindDirection)
	case inputPosition:
		add("$input.position", valueKindPosition)
	case inputEntity:
		add("$input.target", valueKindEntity)
	case inputDirectionPosition:
		add("$input.direction", valueKindDirection)
		add("$input.position", valueKindPosition)
	case inputEntityPosition:
		add("$input.target", valueKindEntity)
		add("$input.position", valueKindPosition)
	case inputTwoPoint:
		add("$input.start_position", valueKindPosition)
		add("$input.end_position", valueKindPosition)
	case inputDrag:
		add("$input.start_position", valueKindPosition)
		add("$input.end_position", valueKindPosition)
		add("$input.drag_direction", valueKindDirection)
		layout.Slots["$input.drag_length"] = valueType{Base: valueKindInt, Quantity: quantityWorldDistance}
	case inputPath:
		add("$input.path", valueKindPath)
		add("$input.start_position", valueKindPosition)
		add("$input.end_position", valueKindPosition)
	default:
		context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema", "unsupported input layout")
	}
	validateInputLayout(context, input, &layout)
	context.artifacts.input = layout
	runStatePass(context)
	runAbilityPass(context)
}

func validateInputLayout(context *compileContext, input inputIR, layout *InputLayout) {
	if layout.HasMaximumRange && layout.MaximumRange <= 0 {
		context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema.maximum_range", "maximum_range must be positive")
	}
	positionClamp := layout.ClampPolicy == "reject" || layout.ClampPolicy == "clamp_end" || layout.ClampPolicy == "nearest_valid"
	pathClamp := positionClamp || layout.ClampPolicy == "clamp_each_segment"
	switch input.kind {
	case inputPosition, inputDirectionPosition, inputEntityPosition:
		if !positionClamp {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema.clamp_policy", "invalid position clamp policy")
		}
	case inputTwoPoint, inputDrag:
		if !layout.HasMaximumRange || layout.MinimumLength < 0 || layout.MaximumLength <= 0 || layout.MinimumLength > layout.MaximumLength {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema", "invalid two-point length bounds")
		}
		if !positionClamp {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema.clamp_policy", "invalid two-point clamp policy")
		}
	case inputPath:
		if layout.MaximumPathPoints < 2 || layout.MaximumPathLength <= 0 || layout.MinimumSegmentLength <= 0 || layout.MinimumSegmentLength > layout.MaximumPathLength {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema", "invalid path bounds")
		}
		if layout.SimplificationPolicy != "reject" && layout.SimplificationPolicy != "drop_short_segments" {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema.simplification_policy", "invalid path simplification policy")
		}
		if !pathClamp {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.input_schema.clamp_policy", "invalid path clamp policy")
		}
	}
	for _, phase := range context.artifacts.ir.phases {
		if phase.events.directionChanged != nil {
			if _, ok := layout.Slots["$input.direction"]; !ok {
				context.addDiagnostic(DiagnosticInputUnavailable, "$.input_schema", "direction_changed at "+phase.events.directionChanged.sourceRef().Path+" requires a direction input slot")
			} else {
				layout.UpdatePorts[InputPortDirectionChanged] = true
			}
		}
		if phase.events.targetChanged != nil {
			if _, ok := layout.Slots["$input.target"]; !ok {
				context.addDiagnostic(DiagnosticInputUnavailable, "$.input_schema", "target_changed at "+phase.events.targetChanged.sourceRef().Path+" requires an entity input slot")
			} else {
				layout.UpdatePorts[InputPortTargetChanged] = true
			}
		}
	}
}
