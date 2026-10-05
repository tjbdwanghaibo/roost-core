package global

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// A retried CompleteMigration at the same epoch is a replay, and is reported
// as one. Counting it as an acceptance makes the migration rate read higher
// than the number of migrations, which is the number operators alarm on.
func TestARetriedCompletionIsReportedAsAReplay(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()
	binding := bind(t, service, 7, 100)
	moving, err := service.BeginMigration(ctx, 7, 200, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	done, err := service.CompleteMigration(ctx, 7, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.CompleteMigration(ctx, 7, done.Epoch)
	if err != nil || again.Epoch != done.Epoch || again.GlobalSID != 200 {
		t.Fatalf("retried completion: err=%v binding=%+v", err, again)
	}
	if got := sink.Count("accepted:complete_migration"); got != 1 {
		t.Errorf("one migration completed reported %d accepts; %s", got, sink.Events())
	}
	if got := sink.Count("replayed:complete_migration"); got != 1 {
		t.Errorf("a retried completion reported %d replays; %s", got, sink.Events())
	}
}
