package saga

import (
	"encoding/json"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// 协调器侧准入（设计 §6）：初始化 fail-closed、滞回、过期按本机单调时钟（不读参与方写入的时刻）、删除 / 过期清除即下线。

func loadEntry(key string, records uint64) fnats.LogKVEntry {
	raw, _ := json.Marshal(participantLoad{V: WireVersion, Records: records})
	return fnats.LogKVEntry{Key: key, Value: raw}
}

func TestAdmissionIsClosedUntilTheInitialSnapshotIsRead(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	a := newAdmission(AdmissionOptions{Enabled: true}.withDefaults(), clock.Now)
	if s := a.evaluate(); s.Open || s.Reason != AdmissionReasonNotReady {
		t.Fatalf("before the snapshot: %+v", s)
	}
	// 初始快照里已有一个超高水位的信号：读完时按积压关闭，不先打开。
	a.observe(loadEntry("part.game.1", 6000))
	if s := a.evaluate(); s.Open || s.Reason != AdmissionReasonNotReady || s.BacklogRecords != 6000 {
		t.Fatalf("snapshot not finished: %+v", s)
	}
	a.markReady()
	if s := a.evaluate(); s.Open || s.Reason != AdmissionReasonBacklog {
		t.Fatalf("ready with a backlog over the high mark: %+v", s)
	}
	empty := newAdmission(AdmissionOptions{Enabled: true}.withDefaults(), clock.Now)
	empty.markReady()
	if s := empty.evaluate(); !s.Open {
		t.Fatalf("ready without any signal must open: %+v", s)
	}
}

func TestAdmissionHysteresisAndDeletion(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	a := newAdmission(AdmissionOptions{Enabled: true, HighRecords: 100, LowRecords: 50}.withDefaults(), clock.Now)
	a.markReady()
	steps := []struct {
		records uint64
		open    bool
	}{{10, true}, {99, true}, {100, false}, {80, false}, {51, false}, {50, true}, {99, true}}
	for i, step := range steps {
		a.observe(loadEntry("part.game.1", step.records))
		if s := a.evaluate(); s.Open != step.open {
			t.Fatalf("step %d (%d records): %+v, want open=%v", i, step.records, s, step.open)
		}
	}
	// 积压由最大的参与方决定；它下线（删除或过期清除，LimitMarkerTTL 的删除标记）后不再计入。
	a.observe(loadEntry("part.game.2", 500))
	if s := a.evaluate(); s.Open {
		t.Fatalf("second participant over the high mark: %+v", s)
	}
	a.observe(fnats.LogKVEntry{Key: "part.game.2", Deleted: true})
	if s := a.evaluate(); s.Open || s.BacklogRecords != 99 {
		t.Fatalf("after the participant left the backlog (99) is still above the low mark: %+v", s)
	}
	a.observe(loadEntry("part.game.1", 40))
	if s := a.evaluate(); !s.Open || s.BacklogRecords != 40 {
		t.Fatalf("below the low mark: %+v", s)
	}
	// admit 提示与坏值不是信号。
	a.observe(fnats.LogKVEntry{Key: admitHintKey, Value: []byte(`{"v":2,"open":false}`)})
	a.observe(fnats.LogKVEntry{Key: "part.game.3", Value: []byte("garbage")})
	if s := a.evaluate(); !s.Open {
		t.Fatalf("hint or garbage counted: %+v", s)
	}
}

// 过期按本机时钟上“最后一次收到更新”的时刻判定：参与方的值不变但持续写入时不过期，停写超过 StaleAfter 关闭，恢复写入后按积压重开。
func TestAdmissionStalenessUsesTheLocalClock(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	a := newAdmission(AdmissionOptions{Enabled: true, HighRecords: 100, LowRecords: 50, StaleAfter: 2 * time.Second}.withDefaults(), clock.Now)
	a.markReady()
	a.observe(loadEntry("part.game.1", 60))
	clock.Advance(1500 * time.Millisecond)
	a.observe(loadEntry("part.game.1", 60)) // 同样的值再写一次：刷新
	clock.Advance(1500 * time.Millisecond)
	if s := a.evaluate(); !s.Open {
		t.Fatalf("refreshed signal judged stale: %+v", s)
	}
	clock.Advance(time.Second)
	if s := a.evaluate(); s.Open || s.Reason != AdmissionReasonStale {
		t.Fatalf("signal not updated for 2.5s: %+v", s)
	}
	a.observe(loadEntry("part.game.1", 60)) // 恢复：积压在滞回区间但没到高水位 → 打开
	if s := a.evaluate(); !s.Open {
		t.Fatalf("fresh again: %+v", s)
	}
}

func TestAdmissionHintsAreOnlyTrustedWhileFresh(t *testing.T) {
	h := &admissionHints{freshness: 50 * time.Millisecond}
	if closed, _ := h.closed(); closed {
		t.Fatal("no hint must not block")
	}
	raw, _ := json.Marshal(admissionHint{V: WireVersion, Open: false, Reason: AdmissionReasonBacklog})
	h.observe(fnats.LogKVEntry{Key: admitHintKey, Value: raw})
	if closed, reason := h.closed(); !closed || reason != AdmissionReasonBacklog {
		t.Fatalf("fresh closed hint: %v %q", closed, reason)
	}
	time.Sleep(60 * time.Millisecond)
	if closed, _ := h.closed(); closed {
		t.Fatal("a stale hint must not block (the owner's verdict is authoritative)")
	}
}

func TestLoadBucketExpiresSignalsVisibly(t *testing.T) {
	config := loadBucketConfig(wireLayout(), 3, AdmissionOptions{}.withDefaults())
	if config.MaxAge != 30*time.Second || config.LimitMarkerTTL != config.MaxAge || config.Bucket != "W_SAGA_LOAD" {
		t.Fatalf("load bucket config = %+v (LimitMarkerTTL is required: without it expiry is silent to Watch)", config)
	}
}
