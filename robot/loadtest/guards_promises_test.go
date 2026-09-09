package loadtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
)

// U-0148 · C2 · gap map core `robot/loadtest` 9/11：管理命令注册缺注册表 / 管理器拒绝；nil
// 管理器的 Start / StartAdmin / Report 报错；无已完成运行时 Report 拒绝、未知 RunID 拒绝；
// 无 profile 名（且无 ActiveProfile）报 ErrProfileRequired，未知 profile 报 ErrProfileNotFound。
func TestManagerEntryPointsRefuseMissingPartsAndUnknownRuns(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRunner{stats: runner.Stats{Started: 1, Success: 1}}
	m := newManager(t, fake, nil, time.Millisecond)
	if err := loadtest.RegisterAdminCommands(nil, m); err == nil || !strings.Contains(err.Error(), "admin registry is required") {
		t.Fatalf("RegisterAdminCommands(nil registry) = %v", err)
	}
	if err := loadtest.RegisterAdminCommands(admin.NewRegistry(), nil); err == nil || !strings.Contains(err.Error(), "manager is required") {
		t.Fatalf("RegisterAdminCommands(nil manager) = %v", err)
	}
	var none *loadtest.Manager
	if _, err := none.Start(ctx, loadtest.StartRequest{Profile: "smoke"}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("Start on a nil manager = %v", err)
	}
	if _, err := none.StartAdmin(ctx, loadtest.StartRequest{Profile: "smoke"}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("StartAdmin on a nil manager = %v", err)
	}
	if _, err := none.Report(loadtest.ReportRequest{}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("Report on a nil manager = %v", err)
	}
	if _, err := m.Report(loadtest.ReportRequest{}); err == nil || !strings.Contains(err.Error(), "no finished runs") {
		t.Fatalf("Report before any run = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{}); !errors.Is(err, loadtest.ErrProfileRequired) {
		t.Fatalf("Start without a profile = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{Profile: "nope"}); !errors.Is(err, loadtest.ErrProfileNotFound) || !strings.Contains(err.Error(), "available=") {
		t.Fatalf("Start with an unknown profile = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, m)
	if _, err := m.Report(loadtest.ReportRequest{RunID: "ghost"}); err == nil || !strings.Contains(err.Error(), `run "ghost" not in history`) {
		t.Fatalf("Report of an unknown run = %v", err)
	}
	if report, err := m.Report(loadtest.ReportRequest{RunID: done.RunID}); err != nil || report["markdown"] == "" {
		t.Fatalf("Report of the finished run = (%v, %v)", report, err)
	}
}
