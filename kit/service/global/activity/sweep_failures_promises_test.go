package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// U-0120 · C5（静默吞错）· classscan C5 扫描。
//
// 后台 sweep 的失败只打日志、在下一个 tick 重试，这是对的——但没有计数：一个
// 每个 tick 都失败的 sweep 在指标上与一个无事可做的 sweep 完全一样（U-0037 在
// outbox 上修过同一形态）。失败必须落到服务的指标汇。

type failingWindows struct {
	versionstore.Store[string, Window]
	err error
}

func (w failingWindows) Get(context.Context, string) (versionstore.Versioned[Window], bool, error) {
	return versionstore.Versioned[Window]{}, false, w.err
}

func TestSweepFailuresAreCountedNotJustLogged(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("windows store: connection reset")
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Windows = failingWindows{Store: cfg.Windows, err: wire}
		cfg.Metrics = recorder
	})
	server := &Server{service: service}
	ctx := context.Background()
	server.sweepGroup(ctx, service, "group-a")
	server.sweepGroup(ctx, service, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("sweep.advance_failed counted %d times after two failing sweeps, want 2 (events: %s)", got, recorder.Events())
	}

	// 健康的 sweep 不计失败。
	healthy, _ := newActivityService(t, func(cfg *Config) { cfg.Metrics = recorder })
	(&Server{service: healthy}).sweepGroup(ctx, healthy, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("a healthy sweep changed the failure count to %d", got)
	}
}
