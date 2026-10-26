package physicsroom

import (
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	"slices"
	"time"
)

// updateGrabs 只从物理执行器调用，基于上一已提交帧验证距离。每个控制者至多
// 一条约束；松手、过期、离开都会释放。拒绝非法目标不终止房间、不允许客户端造关节。
func (s *Service) updateGrabs(r *room) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	poses := make(map[uint64]physx.State, len(r.states))
	for _, v := range r.states {
		poses[v.ID] = v
	}
	dynamic := make(map[uint64]bool, len(r.spec.Bodies))
	for _, b := range r.spec.Bodies {
		dynamic[b.ID] = b.MotionType() == physx.Dynamic
	}
	desired := make(map[uint64]uint64)
	now := time.Now()
	for _, m := range r.members {
		target := m.input.GrabTarget
		if target == 0 || target == m.body || !dynamic[target] || now.Sub(m.inputAt) > s.cfg.InputTTL || now.Sub(m.seenAt) > s.cfg.MemberTTL {
			continue
		}
		a, aok := poses[m.body]
		b, bok := poses[target]
		if !aok || !bok {
			continue
		}
		distance := float32(0)
		for i := range 3 {
			d := a.Position[i] - b.Position[i]
			distance += d * d
		}
		if distance > 2.5*2.5 {
			continue
		}
		desired[m.body] = target
	}
	for body, target := range r.grabs {
		if desired[body] != target {
			if err := r.world.RemoveJoint(body | 1<<63); err != nil {
				return err
			}
			delete(r.grabs, body)
		}
	}
	keys := make([]uint64, 0, len(desired))
	for body := range desired {
		keys = append(keys, body)
	}
	slices.Sort(keys)
	for _, body := range keys {
		target := desired[body]
		if r.grabs[body] == target {
			continue
		}
		if err := r.world.AddJoint(physx.Joint{ID: body | 1<<63, A: body, B: target, Kind: physx.Distance, MaxDistance: 1.2, Stiffness: 100, Damping: 10, BreakForce: 300}); err != nil {
			return err
		}
		r.grabs[body] = target
	}
	return nil
}
