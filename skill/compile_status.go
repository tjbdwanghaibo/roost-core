package skill

func runStatusInstancePass(context *compileContext) {
	if context.artifacts.ir == nil {
		return
	}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		selectFlow, ok := flow.(*selectFlowIR)
		if !ok {
			return
		}
		plan := &selectFlow.selectPlan
		_, statusShape := plan.shape.(*statusSetShapeIR)
		if statusShape != (plan.elementType == selectionStatusInstance) {
			context.addDiagnostic(DiagnosticShapeInvalid, plan.source.Path, "status instance selects require kind=status_instance and shape=status_set")
		}
		if !statusShape {
			return
		}
		for _, filter := range plan.filters {
			statusFilter, valid := filter.(*statusInstanceFilterIR)
			if !valid {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "filter is not valid for a status instance select")
				continue
			}
			if statusFilter.kind == "status_id" && context.artifacts.authority.statuses[statusFilter.status] == 0 {
				context.addDiagnostic(DiagnosticCapabilityUnknown, statusFilter.source.Path, "unknown status id")
			}
			if statusFilter.kind == "status_tag" && context.artifacts.authority.tags[statusFilter.text] == 0 {
				context.addDiagnostic(DiagnosticCapabilityUnknown, statusFilter.source.Path, "unknown status gameplay tag")
			}
			if statusFilter.kind == "status_tag" {
				handle := context.artifacts.authority.tags[statusFilter.text]
				queryable := false
				for _, tag := range context.environment.Gameplay.Tags.Entries {
					if tag.Handle == handle {
						queryable = tag.Classes&GameplayTagTargetQueryable != 0
						break
					}
				}
				if handle != 0 && !queryable {
					context.addDiagnostic(DiagnosticCapabilityUnknown, statusFilter.source.Path, "status gameplay tag is not target-queryable")
				}
			}
			switch statusFilter.kind {
			case "status_category", "status_source_skill":
				if statusFilter.text == "" {
					context.addDiagnostic(DiagnosticShapeInvalid, statusFilter.source.Path, "status filter value is required")
				}
			case "status_polarity":
				if statusFilter.text != "positive" && statusFilter.text != "negative" && statusFilter.text != "neutral" {
					context.addDiagnostic(DiagnosticShapeInvalid, statusFilter.source.Path, "invalid status polarity")
				}
			case "status_source", "status_owner":
				if statusFilter.value == nil {
					context.addDiagnostic(DiagnosticShapeInvalid, statusFilter.source.Path, "status entity filter requires an entity")
				}
			case "status_stack_compare", "status_duration_compare":
				if statusFilter.value == nil || !validCompareOperation(statusFilter.operation) {
					context.addDiagnostic(DiagnosticShapeInvalid, statusFilter.source.Path, "invalid status comparison")
				}
			}
		}
		if plan.limit > context.environment.Limits.MaxStatusSelections {
			context.addDiagnostic(DiagnosticBudgetExceeded, plan.source.Path+".limit", "status selection exceeds environment maximum")
		}
		if plan.order != nil && !validStatusOrder(plan.order.by, plan.order.direction) {
			context.addDiagnostic(DiagnosticShapeInvalid, plan.source.Path+".order", "invalid status instance order")
		}
		var consumer flowIR
		switch consume := selectFlow.consume.(type) {
		case *selectOneConsumeIR:
			consumer = consume.then
		case *selectEachConsumeIR:
			consumer = consume.body
		}
		if effectResultBranchMaySuspend(consumer) {
			context.addDiagnostic(DiagnosticLifecycleControlConflict, consumer.sourceRef().Path, "status instance consumers cannot suspend or create a process")
		}
	})
	context.artifacts.ir.walkEffects(func(effect effectIR) {
		mutation, ok := effect.(*modifyStatusInstanceEffectIR)
		if !ok {
			return
		}
		valid := false
		switch mutation.operation {
		case "remove", "refresh":
			valid = mutation.value == nil && mutation.target == nil
		case "add_stacks", "set_stacks", "add_duration", "set_duration", "mul_duration_bp":
			valid = mutation.value != nil && mutation.target == nil
		case "copy_to", "transfer_to":
			valid = mutation.value == nil && mutation.target != nil && validStatusOwnershipPolicy(mutation.ownershipPolicy)
		}
		if !valid {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path, "status instance operation arguments are invalid")
		}
	})
}

func validStatusOrder(order, direction string) bool {
	if direction != "asc" && direction != "desc" {
		return false
	}
	switch order {
	case "status_dispel_priority", "remaining_duration", "stack_count", "applied_tick", "status_instance_id":
		return true
	default:
		return false
	}
}

func validStatusOwnershipPolicy(policy string) bool {
	switch policy {
	case "original_owner", "new_owner", "original_source", "new_source":
		return true
	default:
		return false
	}
}

func validCompareOperation(operation string) bool {
	switch operation {
	case "eq", "ne", "lt", "lte", "gt", "gte":
		return true
	default:
		return false
	}
}
