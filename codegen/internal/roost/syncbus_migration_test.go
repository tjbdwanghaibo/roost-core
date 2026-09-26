package roost

import (
	"os"
	"strings"
	"testing"
)

func TestConsolidationMapSyncBusLegacyPaths(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	file := writeProjectFile(t, root, "wiring.go", `package wiring
import (
 oldkit "github.com/tjbdwanghaibo/roost-core/kit/room"
 olddriver "github.com/tjbdwanghaibo/roost-core/room"
)
var _ = oldkit.NewRoomMod
var _ oldkit.RoomMod
var _ = olddriver.NewNatsSyncBus
`)
	_, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"roost-core/kit/syncbus", "roost-core/sync/syncbus/driver", "oldkit.NewSyncBusMod", "oldkit.SyncBusMod"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}
