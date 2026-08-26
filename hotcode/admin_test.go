package hotcode

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/admin"
)

func TestRegisterAdminCommandsTargetsInstanceRegistry(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	if err := MustRegisterHelper(t); err != nil {
		t.Fatal(err)
	}

	reg := admin.NewRegistry()
	if err := RegisterAdminCommands(reg); err != nil {
		t.Fatalf("RegisterAdminCommands: %v", err)
	}
	// The assembled ops surface executes through the instance registry —
	// the commands must be reachable there, not only on the package default.
	result, err := reg.Execute(context.Background(), admin.Command{Name: AdminCommandList})
	if err != nil || !result.OK {
		t.Fatalf("execute %s: result=%+v err=%v", AdminCommandList, result, err)
	}
	points, ok := result.Data["points"].([]PointInfo)
	if !ok || len(points) != 1 || points[0].Name != "demo.point" {
		t.Fatalf("points = %+v", result.Data["points"])
	}

	if err := RegisterAdminCommands(nil); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func MustRegisterHelper(t *testing.T) error {
	t.Helper()
	return Register("demo.point", func() int { return 1 })
}
