package physicsroom

import (
	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"strings"
	"testing"
)

func TestReplicationFrameMetricsCountEncodedFields(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	var c chunksync.Cursor
	snapshot := chunksync.Snapshot{Epoch: 1, Tick: 1, Objects: []chunksync.Object{{ID: 1, Data: make([]byte, 34)}}}
	p, err := c.PlanFields(snapshot, gamephysics.QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	recordFrame(p.Data)
	_ = c.Commit(p)
	snapshot.Tick++
	snapshot.Objects[0].Data[0] = 1
	p, err = c.PlanFields(snapshot, gamephysics.QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	recordFrame(p.Data)
	_ = c.Commit(p)
	snapshot.Tick++
	snapshot.Objects = nil
	p, err = c.PlanFields(snapshot, gamephysics.QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	recordFrame(p.Data)
	text := string(metrics.PrometheusText(reg.Snapshot()))
	for _, want := range []string{`physics_room_frames_total{mode="full"} 1`, `physics_room_frames_total{mode="delta"} 2`, `physics_room_fields_total{field="position"} 2`, `physics_room_fields_total{field="velocity"} 1`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
}
