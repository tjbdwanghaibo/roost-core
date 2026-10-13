package skill

import "fmt"

func (host *MemoryHost) applyMotionStepLocked(step MotionStep, state *SpawnHostState) ([]SpawnSignal, bool, error) {
	switch typed := step.(type) {
	case StaticMotionStep:
		state.Position, state.Active = typed.Position, true
		return nil, true, nil
	case FrameMotionStep:
		state.Position, state.Active = typed.Position, true
	case SteeringMotionStep:
		// 方向归衍生物所有；Host 收到已解析的整数原语用于确定性追踪，但不保留 DSL 状态。
	case TrajectoryMotionStep:
		state.Position = typed.Position
	case OffsetsMotionStep:
		state.Position = typed.Position
	case CollisionMotionStep:
		state.Position = typed.Position
		_ = append([]CollisionLayerHandle(nil), typed.Layers...)
	case CarryMotionStep:
		if typed.Attached && typed.Target != 0 {
			entity, found := host.entities[typed.Target]
			if !found || !entity.Alive {
				return []SpawnSignal{{Kind: SpawnSignalTargetLost, Target: typed.Target}}, false, nil
			}
			entity.Position = typed.Position
			host.entities[typed.Target] = entity
		}
	case CompletionMotionStep:
		if typed.Complete {
			state.Active = false
		}
	case SignalsMotionStep:
		return normalizeSpawnSignals(typed.Signals), true, nil
	default:
		return nil, false, fmt.Errorf("skill: unsupported motion step %T", step)
	}
	return nil, false, nil
}
