//go:build physx && cgo

package physics

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// 同一次 PhysX 运行的帧分别编码，避免两次模拟轨迹差异污染带宽对照。
// 模拟时间 20s，物理 50Hz，同步 20Hz，20 个观察者，12/14m AOI；不是网络吞吐压测。
func TestRoomBandwidthSamePhysicsFrames(t *testing.T) {
	w, err := physx.New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	scene := CrowdScene()
	for _, b := range scene {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var old, private, quantized, fields [20]chunksync.Cursor
	var replicas [20]RoomReplica
	var visible [20]map[uint64]bool
	var totals [5]int64
	var optimized [20]chunksync.Cursor
	var views [20]map[uint64]ViewEntry
	maxPositionError := float64(0)
	frames := 0
	objects := 0
	for tick := uint64(1); tick <= 1000; tick++ {
		commands := CrowdCommands(tick)
		for i := range 20 {
			a := float64(i)*2*math.Pi/20 + float64(tick)/150
			commands = append(commands, physx.Command{ID: uint64(51 + i), Acceleration: physx.Vec3{float32(8 * math.Cos(a)), 0, float32(8 * math.Sin(a))}})
		}
		states, err := w.Step(.02, commands)
		if err != nil {
			t.Fatal(err)
		}
		if tick%5 != 0 && tick%5 != 2 {
			continue
		}
		frames++
		for i := range states {
			if states[i].ID >= 51 && states[i].ID <= 70 {
				states[i].InputSequence = tick*2/5 + 1
				states[i].InputGeneration = 100 + states[i].ID
				states[i].InputActive = true
			}
		}
		frame := Frame{Epoch: 1, Tick: tick, States: states}
		raw, err := NewSpatialFrame(frame, 25, 10)
		if err != nil {
			t.Fatal(err)
		}
		packed, err := NewQuantizedSpatialFrame(frame, 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 20 {
			observer := uint64(i + 51)
			a, next, err := raw.Visible(observer, 12, 2, visible[i])
			if err != nil {
				t.Fatal(err)
			}
			b, _, err := packed.Visible(observer, 12, 2, visible[i])
			if err != nil {
				t.Fatal(err)
			}
			visible[i] = next
			objects += len(a.Objects)
			p, err := old[i].Plan(a)
			if err != nil {
				t.Fatal(err)
			}
			totals[0] += int64(len(p.Data))
			_ = old[i].Commit(p)
			stripped := chunksync.Snapshot{Epoch: a.Epoch, Tick: a.Tick, Objects: make([]chunksync.Object, len(a.Objects))}
			for j, o := range a.Objects {
				o.Data = o.Data[:53]
				stripped.Objects[j] = o
			}
			p, err = private[i].Plan(stripped)
			if err != nil {
				t.Fatal(err)
			}
			totals[1] += int64(len(p.Data) + RoomHeaderBytes)
			_ = private[i].Commit(p)
			p, err = quantized[i].Plan(b)
			if err != nil {
				t.Fatal(err)
			}
			totals[2] += int64(len(p.Data) + RoomHeaderBytes)
			_ = quantized[i].Commit(p)
			p, err = fields[i].PlanFields(b, QuantizedLayout())
			if err != nil {
				t.Fatal(err)
			}
			packet := RoomPayload(p.Data, InputAck{Sequence: tick*2/5 + 1, Generation: 100 + observer, Active: true})
			totals[3] += int64(len(packet))
			if err = replicas[i].Apply(packet); err != nil {
				t.Fatal(err)
			}
			_ = fields[i].Commit(p)
			view, err := PlanView(b, states, scene, nil, observer, nil, views[i], DefaultReplicationPolicy())
			if err != nil {
				t.Fatal(err)
			}
			optimizedPlan, err := optimized[i].PlanFields(view.Snapshot, QuantizedLayout())
			if err != nil {
				t.Fatal(err)
			}
			totals[4] += int64(len(optimizedPlan.Data) + RoomHeaderBytes)
			if err = optimized[i].Commit(optimizedPlan); err != nil {
				t.Fatal(err)
			}
			views[i] = view.Entries

			for _, state := range states {
				if decoded, ok := replicas[i].States[state.ID]; ok {
					for axis := range 3 {
						e := math.Abs(float64(decoded.Position[axis] - state.Position[axis]))
						maxPositionError = math.Max(maxPositionError, e)
					}
				}
			}
		}
	}
	if totals[3] >= totals[0]*3/4 {
		t.Fatalf("insufficient reduction %v", totals)
	}
	if maxPositionError > .0051 {
		t.Fatal("position error", maxPositionError)
	}
	report := struct {
		Frames, Observers                                                                int
		MeanVisible                                                                      float64
		Bytes                                                                            [5]int64
		ReductionPercent, OldKBpsPerClient, NewKBpsPerClient, MaxAxisPositionErrorMeters float64
	}{frames, 20, float64(objects) / float64(frames*20), totals, 100 * (1 - float64(totals[3])/float64(totals[0])), float64(totals[0]) / 20 / 20 / 1000, float64(totals[3]) / 20 / 20 / 1000, maxPositionError}
	if totals[4] >= totals[3] {
		t.Fatalf("view optimization did not reduce bytes: %v", totals)
	}
	data, _ := json.Marshal(report)
	t.Log("BANDWIDTH_RESULT " + string(data))
	t.Logf("VIEW_RESULT bytes=%d kBps_per_client=%.5f reduction_from_fields_percent=%.3f", totals[4], float64(totals[4])/400000, 100*(1-float64(totals[4])/float64(totals[3])))
}

func TestMotionReplicationSameNativeFrames(t *testing.T) {
	bodies := []physx.Body{
		{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{25, .5, 25}},
		{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{12, .5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1},
		{ID: 2, Shape: physx.Box, Motion: physx.Kinematic, Position: physx.Vec3{0, 2, 0}, Dimensions: physx.Vec3{1, .2, 1}, Mass: 10},
	}
	tracks := []MotionTrack{{ID: 2, Translation: physx.Vec3{0, 1, 0}, PeriodSeconds: 4, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1}}
	w, err := physx.New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range bodies {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var baseline, optimized chunksync.Cursor
	var entries map[uint64]ViewEntry
	var replica RoomReplica
	var oldBytes, newBytes int
	maxAge := uint64(0)
	for tick := uint64(1); tick <= 500; tick++ {
		states, err := w.Step(StepSeconds, MotionCommands(bodies, tracks, tick))
		if err != nil {
			t.Fatal(err)
		}
		if tick%5 != 0 && tick%5 != 2 {
			continue
		}
		snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		if err != nil {
			t.Fatal(err)
		}
		p, err := baseline.PlanFields(snapshot, QuantizedLayout())
		if err != nil {
			t.Fatal(err)
		}
		oldBytes += len(p.Data) + RoomHeaderBytes
		_ = baseline.Commit(p)
		view, err := PlanView(snapshot, states, bodies, tracks, 1, nil, entries, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if view.Entries[2].Class != "motion" {
			t.Fatal("motion not throttled")
		}
		age := tick - view.Entries[2].Tick
		if age > maxAge {
			maxAge = age
		}
		if age >= 50 {
			t.Fatal("calibration overdue")
		}
		p, err = optimized.PlanFields(view.Snapshot, QuantizedLayout())
		if err != nil {
			t.Fatal(err)
		}
		packet := RoomPayload(p.Data, InputAck{})
		newBytes += len(packet)
		if err = replica.Apply(packet); err != nil {
			t.Fatal(err)
		}
		_ = optimized.Commit(p)
		entries = view.Entries
		// 原生权威姿态须与已发布轨迹同相位，客户端金样另验证重建公式。
		target := MotionCommands(bodies, tracks, tick)[0]
		for _, state := range states {
			if state.ID == 2 {
				for axis := range 3 {
					if math.Abs(float64(state.Position[axis]-target.Position[axis])) > .0001 {
						t.Fatal("native motion phase differs")
					}
				}
			}
		}
	}
	if newBytes >= oldBytes*3/4 {
		t.Fatalf("motion savings insufficient: %d -> %d", oldBytes, newBytes)
	}
	t.Logf("MOTION_RESULT baseline=%d optimized=%d max_sample_age_ticks=%d", oldBytes, newBytes, maxAge)
}
