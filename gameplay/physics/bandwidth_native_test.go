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
	for _, b := range CrowdScene() {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var old, private, quantized, fields [20]chunksync.Cursor
	var replicas [20]RoomReplica
	var visible [20]map[uint64]bool
	var totals [4]int64
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
		Bytes                                                                            [4]int64
		ReductionPercent, OldKBpsPerClient, NewKBpsPerClient, MaxAxisPositionErrorMeters float64
	}{frames, 20, float64(objects) / float64(frames*20), totals, 100 * (1 - float64(totals[3])/float64(totals[0])), float64(totals[0]) / 20 / 20 / 1000, float64(totals[3]) / 20 / 20 / 1000, maxPositionError}
	data, _ := json.Marshal(report)
	t.Log("BANDWIDTH_RESULT " + string(data))
}
