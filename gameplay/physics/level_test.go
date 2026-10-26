package physics

import (
	"bytes"
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	"testing"
)

func TestLevelContentAndMechanisms(t *testing.T) {
	l := ObstacleLevel()
	data, hash, err := l.Content()
	if err != nil {
		t.Fatal(err)
	}
	copy, err := DecodeLevel(data)
	if err != nil {
		t.Fatal(err)
	}
	again, h, err := copy.Content()
	if err != nil || h != hash || !bytes.Equal(data, again) {
		t.Fatal("unstable content", err)
	}
	c := MotionCommands(l.Bodies, l.Motions, 75)
	if len(c) != 2 || c[0].Position[1] < 1.99 || !c[0].Target || c[1].Rotation[1] < .6 {
		t.Fatalf("platform/rotor: %+v", c)
	}
	changed := l
	changed.Revision++
	_, other, _ := changed.Content()
	if other == hash {
		t.Fatal("revision missing from content identity")
	}
	for _, bad := range [][]byte{append(data, []byte("{}")...), bytes.Replace(data, []byte(`"Version":1`), []byte(`"Version":9`), 1), bytes.Replace(data, []byte(`"Version":1`), []byte(`"Unknown":1,"Version":1`), 1)} {
		if _, err := DecodeLevel(bad); err == nil {
			t.Fatal("invalid content accepted")
		}
	}
	l.Players = []uint64{4}
	if l.Validate() == nil {
		t.Fatal("kinematic controller accepted")
	}
	l = ObstacleLevel()
	l.Motions[0].ID = 1
	if l.Validate() == nil {
		t.Fatal("dynamic target accepted")
	}
	l = ObstacleLevel()
	l.Joints[0].B = 999
	if l.Validate() == nil {
		t.Fatal("missing joint actor")
	}
	l = ObstacleLevel()
	l.Bodies[1].Mass = 1e30
	if l.Validate() == nil {
		t.Fatal("extreme mass accepted")
	}
	l = ObstacleLevel()
	l.Bodies[0].ID = 1
	if l.Validate() == nil {
		t.Fatal("duplicate ID")
	}
}
func TestInitialFrameIncludesMovingBoxes(t *testing.T) {
	f := InitialFrame(1, ObstacleLevel().Bodies)
	seen := map[uint64]bool{}
	for _, v := range f.States {
		seen[v.ID] = true
	}
	if seen[100] || !seen[4] || !seen[5] || !seen[6] {
		t.Fatal(seen)
	}
	if (physx.Body{Shape: physx.Box}).MotionType() != physx.Static {
		t.Fatal("legacy static box changed")
	}
}

func TestSnapshotCarriesSampledInputIdentity(t *testing.T) {
	f := Frame{Epoch: 1, Tick: 2, States: []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}, InputSequence: 7, InputGeneration: 9, InputActive: true}}}
	snapshot, err := Snapshot(f, 25)
	if err != nil {
		t.Fatal(err)
	}
	data := snapshot.Objects[0].Data
	if len(data) != 70 || binary.LittleEndian.Uint64(data[53:]) != 7 || data[61] != 1 || binary.LittleEndian.Uint64(data[62:]) != 9 {
		t.Fatalf("sample identity missing: %v", data)
	}
}
