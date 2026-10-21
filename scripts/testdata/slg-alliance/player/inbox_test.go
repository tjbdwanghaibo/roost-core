package alliance

import (
	"context"
	"encoding/json"
	"errors"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// 第三组崩溃断点：Player 已 durable、地图尚未 ACK。冷读必须先于任何重投。
func runInboxChild(t *testing.T, n *rewardRuntime, sid int32, mode string) {
	ctx := context.Background()
	s := sender.NewAllianceRewardsSender(n.nest)
	root := os.Getenv("ROOST_SLG_REWARD_WAL")
	report, err := s.Sync_AllianceReport(ctx, n.mapID, "march")
	check(t, err)
	inbox, err := s.Sync_AllianceInbox(ctx, n.id)
	check(t, err)
	mapRaw, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
	check(t, err)
	wallet, err := s.Sync_AllianceWallet(ctx, n.id)
	check(t, err)
	type facts struct{ Inbox, Map string }
	snapshot := func(name string) {
		b, err := json.Marshal(facts{inbox, mapRaw})
		check(t, err)
		check(t, os.WriteFile(filepath.Join(root, name+".tmp"), b, 0600))
		check(t, os.Rename(filepath.Join(root, name+".tmp"), filepath.Join(root, name)))
	}
	if mode != "inbox-write" {
		marker := "inbox-ready.json"
		if mode == "inbox-verify" {
			marker = "inbox-acked.json"
		}
		raw, err := os.ReadFile(filepath.Join(root, marker))
		check(t, err)
		var cold facts
		check(t, json.Unmarshal(raw, &cold))
		if cold != (facts{inbox, mapRaw}) {
			t.Fatal("inbox/map cold facts differ BEFORE retry")
		}
	}
	pending, err := s.Sync_AlliancePendingReports(ctx, n.mapID, 1)
	check(t, err)
	if mode == "inbox-verify" {
		if len(pending) != 0 {
			t.Fatal("ack lost after restart")
		}
	} else if !slices.Equal(pending, []string{"march"}) {
		t.Fatal("unacknowledged report lost", pending)
	}
	if mode == "inbox-write" {
		if inbox != "{}" {
			t.Fatal("unexpected initial inbox", inbox)
		}
		baseline := n.projector.Stats()
		if baseline.WALUnacked != 0 {
			t.Fatal("previous scenario not drained")
		}
		for _, wrong := range []scout.Report{
			func() scout.Report { r := report; r.Recipient++; return r }(),
			func() scout.Report { r := report; r.HomeSID = 999; return r }(),
		} {
			if _, err := s.Sync_AllianceReceiveReport(ctx, n.id, sid, wrong, false); err == nil {
				t.Fatal("wrong recipient/home accepted")
			}
		}
		if _, err := s.Sync_AllianceReceiveReport(ctx, n.id, sid, report, true); err == nil {
			t.Fatal("receipt rollback accepted")
		}
		after, err := s.Sync_AllianceInbox(ctx, n.id)
		check(t, err)
		if after != inbox {
			t.Fatal("receipt rollback changed inbox")
		}
		// worker 在收件提交后丢失响应，不能提前 ACK；重启必须重新读取同一持久义务。
		lost := errors.New("injected lost receive response")
		delivery := reportDelivery(n, sid)
		receive := delivery.Receive
		delivery.Receive = func(ctx context.Context, r scout.Report) error {
			if err := receive(ctx, r); err != nil {
				return err
			}
			return lost
		}
		worker := reportWorker(n, delivery)

		if !errors.Is(worker.Tick(ctx), lost) {
			t.Fatal("lost response not surfaced")
		}
		inbox, err = s.Sync_AllianceInbox(ctx, n.id)
		check(t, err)
		afterMap, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		if afterMap != mapRaw {
			t.Fatal("lost reply acknowledged report")
		}
		stats := n.projector.Stats()
		if stats.WALUnacked != 1 || stats.Projected != baseline.Projected {
			t.Fatal("expected exactly one unprojected Player receipt", stats)
		}
		var receipts map[string]scout.Report
		check(t, json.Unmarshal([]byte(inbox), &receipts))
		if len(receipts) != 1 || receipts[report.Identity()] != report {
			t.Fatal("first receipt missing")
		}
		currentWallet, err := s.Sync_AllianceWallet(ctx, n.id)
		check(t, err)
		if currentWallet != wallet {
			t.Fatal("receipt changed wallet before crash")
		}
		snapshot("inbox-ready.json")
		select {} // 父进程在完整标记出现后 SIGKILL。
	}
	// 多个投递者重试已落地回执，只能得到相同正文，无新增收件效果。
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := s.Sync_AllianceReceiveReport(ctx, n.id, sid, report, false)
			if err == nil && applied {
				err = errors.New("receipt duplicated")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	changed := report
	changed.At++
	if _, err := s.Sync_AllianceReceiveReport(ctx, n.id, sid, changed, false); err == nil {
		t.Fatal("changed immutable report accepted")
	}
	if err := s.Sync_AllianceAckReport(ctx, n.mapID, changed, false); err == nil {
		t.Fatal("changed ack accepted")
	}
	if mode == "inbox-recover" {
		if err := s.Sync_AllianceAckReport(ctx, n.mapID, report, true); err == nil {
			t.Fatal("ack rollback accepted")
		}
		after, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		if after != mapRaw {
			t.Fatal("ack rollback changed map")
		}
	}
	worker := reportWorker(n, reportDelivery(n, sid))
	check(t, worker.Tick(ctx))
	check(t, worker.Tick(ctx))
	check(t, s.Sync_AllianceAckReport(ctx, n.mapID, report, false))
	after, err := s.Sync_AllianceInbox(ctx, n.id)
	check(t, err)
	if after != inbox {
		t.Fatal("retry changed inbox")
	}
	var received map[string]scout.Report
	check(t, json.Unmarshal([]byte(inbox), &received))
	if len(received) != 1 || received[report.Identity()] != report {
		t.Fatal("receipt content incorrect", received)
	}
	afterWallet, err := s.Sync_AllianceWallet(ctx, n.id)
	check(t, err)
	if afterWallet != wallet {
		t.Fatal("report delivery changed wallet")
	}
	pending, err = s.Sync_AlliancePendingReports(ctx, n.mapID, 128)
	check(t, err)
	if len(pending) != 0 {
		t.Fatal("acknowledged report still pending")
	}
	mapRaw, err = s.Sync_AllianceMapSnapshot(ctx, n.mapID)
	check(t, err)
	if mode == "inbox-recover" {
		snapshot("inbox-acked.json")
	}
	t.Log(mode + ": original report received once, map acknowledged, wallet unchanged")
}

func reportDelivery(n *rewardRuntime, sid int32) scout.ReportDelivery {
	s := sender.NewAllianceRewardsSender(n.nest)
	return scout.ReportDelivery{MapID: n.mapID,
		Read: func(ctx context.Context, key string) (scout.Report, error) {
			return s.Sync_AllianceReport(ctx, n.mapID, key)
		},
		Receive: func(ctx context.Context, r scout.Report) error {
			_, err := s.Sync_AllianceReceiveReport(ctx, r.Recipient, sid, r, false)
			return err
		},
		Ack: func(ctx context.Context, r scout.Report) error {
			return s.Sync_AllianceAckReport(ctx, n.mapID, r, false)
		},
	}
}
func reportWorker(n *rewardRuntime, d scout.ReportDelivery) ArrivalWorker {
	return ArrivalWorker{Limit: 1, Discover: func(ctx context.Context, limit int) ([]string, error) {
		return sender.NewAllianceRewardsSender(n.nest).Sync_AlliancePendingReports(ctx, n.mapID, limit)
	}, Consume: d.Deliver}
}
