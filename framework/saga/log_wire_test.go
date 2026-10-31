package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// JetStream 协调器线格式（WireVersion 2）：事件、输入、快照清单与正文的编解码与校验；版本不对、分区 / 布局不符、摘要不符都拒绝。

func wireLayout() Layout { return Layout{Prefix: "w.saga", Stream: "W_SAGA", Partitions: 4, Groups: 2} }

// wireSaga 造一笔属于分区 k 的合法状态。
func wireSaga(t *testing.T, layout Layout, k int) sagaState {
	t.Helper()
	for i := 0; ; i++ {
		id := IDOf("rally", "bk-"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
		if layout.PartitionOf(id) != k {
			continue
		}
		now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
		op := operationKey(id, testLifecycle, PhaseForward, 0)
		return sagaState{
			Record:   Record{Lifecycle: testLifecycle, ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "bk", Status: StatusWaiting, Phase: PhaseForward, Version: 2, Attempt: 1, OperationKey: op, CommandID: commandID(op, 0, 1), NextRunAt: now, CreatedAt: now, UpdatedAt: now, StartAttempt: "a.1"},
			Ops:      map[string]opTombstone{operationKey(id, testLifecycle, PhaseCompensate, 0): {Closure: OperationAbandoned, LateAlarms: []uint32{0}}},
			Receipts: map[string]string{"c1": strings.Repeat("a", 64)},
		}
	}
}

func TestLogEventRoundTripAndValidation(t *testing.T) {
	layout := wireLayout()
	st := wireSaga(t, layout, 1)
	cmd := Command{ID: st.Record.CommandID, IdempotencyKey: st.Record.OperationKey, SagaID: st.Record.ID, SagaType: "rally", DefinitionVersion: 1, BusinessKey: "bk", StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve", DeadlineAt: st.Record.NextRunAt, CreatedAt: st.Record.CreatedAt, Partition: 1}
	ev := logEvent{Part: 1, Writer: "w", In: 7, At: st.Record.UpdatedAt, Kind: "dispatch", Saga: st, Command: &cmd}
	raw, err := encodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeEvent(raw, layout, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != WireVersion || got.In != 7 || got.Saga.Record.StartAttempt != "a.1" || got.Command.Partition != 1 || got.Saga.Ops[operationKey(st.Record.ID, testLifecycle, PhaseCompensate, 0)].LateAlarms[0] != 0 || got.Saga.Receipts["c1"] == "" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	mutate := func(name string, fn func(map[string]any)) {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		fn(doc)
		bad, _ := json.Marshal(doc)
		if _, err := decodeEvent(bad, layout, 1); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: decodeEvent = %v, want ErrInvalidRecord", name, err)
		}
	}
	mutate("wire version 1", func(d map[string]any) { d["v"] = 1 })
	mutate("other partition", func(d map[string]any) { d["part"] = 2 })
	mutate("unknown kind", func(d map[string]any) { d["kind"] = "rejected" })
	mutate("no writer", func(d map[string]any) { d["writer"] = "" })
	mutate("invalid record", func(d map[string]any) { d["saga"].(map[string]any)["record"].(map[string]any)["Version"] = 0 })
	mutate("foreign tombstone", func(d map[string]any) {
		d["saga"].(map[string]any)["ops"] = map[string]any{"other:1:0": map[string]any{"closure": 2}}
	})
	mutate("command of another partition", func(d map[string]any) { d["command"].(map[string]any)["partition"] = 3 })
	if _, err := decodeEvent(raw, layout, 2); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("event read on another partition: %v", err)
	}
}

func TestLogInputsAreValidated(t *testing.T) {
	digest := strings.Repeat("d", 64)
	good, _ := json.Marshal(startInput{V: WireVersion, Request: StartRequest{Type: "rally", BusinessKey: "bk", DefinitionVersion: 1}, Attempt: "i.1", Digest: digest})
	if _, err := decodeStartInput(good); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]startInput{
		"version 1":    {V: 1, Request: StartRequest{Type: "rally"}, Attempt: "i.1", Digest: digest},
		"no attempt":   {V: WireVersion, Request: StartRequest{Type: "rally"}, Digest: digest},
		"short digest": {V: WireVersion, Request: StartRequest{Type: "rally"}, Attempt: "i.1", Digest: "ab"},
		"long attempt": {V: WireVersion, Request: StartRequest{Type: "rally"}, Attempt: strings.Repeat("a", 257), Digest: digest},
	} {
		raw, _ := json.Marshal(in)
		if _, err := decodeStartInput(raw); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("start %s: %v", name, err)
		}
	}
	c := Completion{CommandID: "s:1:0:1", IdempotencyKey: "s:1:0", SagaID: "s", Success: true, Data: []byte("1234")}
	raw, _ := json.Marshal(resultInput{V: WireVersion, Completion: c})
	if _, err := decodeResultInput(raw, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeResultInput(raw, 3); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("oversized result data: %v", err)
	}
	c.Error = "both"
	raw, _ = json.Marshal(resultInput{V: WireVersion, Completion: c})
	if _, err := decodeResultInput(raw, 100); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("success with error: %v", err)
	}
	now := time.Now()
	for name, in := range map[string]opInput{
		"resume without request":    {V: WireVersion, Kind: opResume, ID: "s", Now: now, Attempt: "a"},
		"resume of another id":      {V: WireVersion, Kind: opResume, ID: "s", Resume: &ResumeRequest{ID: "t"}, Now: now, Attempt: "a"},
		"compensate with a request": {V: WireVersion, Kind: opCompensate, ID: "s", Resume: &ResumeRequest{ID: "s"}, Now: now, Attempt: "a"},
		"no time":                   {V: WireVersion, Kind: opCompensate, ID: "s", Attempt: "a"},
		"unknown kind":              {V: WireVersion, Kind: "delete", ID: "s", Now: now, Attempt: "a"},
	} {
		raw, _ := json.Marshal(in)
		if _, err := decodeOpInput(raw); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("op %s: %v", name, err)
		}
	}
}

// 快照加载校验版本 / 分区 / 布局 / 长度 / 摘要，任何一项不符都 ErrSnapshotIncompatible（分区不启动、不退回从头重放）。
func TestSnapshotManifestAndBodyAreChecked(t *testing.T) {
	layout := wireLayout()
	states := []sagaState{wireSaga(t, layout, 1)}
	body, _ := json.Marshal(states)
	sum := sha256.Sum256(body)
	good := snapshotManifest{V: snapshotWireVersion, Part: 1, Layout: layoutOf(layout), LastEv: 10, InHi: 9, Writer: "w", Object: "p1-10", Size: len(body), SHA256: hex.EncodeToString(sum[:])}
	raw, _ := json.Marshal(good)
	manifest, err := decodeManifest(raw, layout, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeSnapshotBody(body, manifest, layout, 1); err != nil || len(got) != 1 {
		t.Fatalf("body: %v %v", got, err)
	}
	for name, fn := range map[string]func(*snapshotManifest){
		"version":    func(m *snapshotManifest) { m.V = 1 },
		"partition":  func(m *snapshotManifest) { m.Part = 2 },
		"partitions": func(m *snapshotManifest) { m.Layout.Partitions = 8 },
		"groups":     func(m *snapshotManifest) { m.Layout.Groups = 1 },
		"prefix":     func(m *snapshotManifest) { m.Layout.Prefix = "other" },
		"no object":  func(m *snapshotManifest) { m.Object = "" },
	} {
		m := good
		fn(&m)
		raw, _ := json.Marshal(m)
		if _, err := decodeManifest(raw, layout, 1); !errors.Is(err, ErrSnapshotIncompatible) {
			t.Errorf("manifest %s: %v", name, err)
		}
	}
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] ^= 1
	if _, err := decodeSnapshotBody(tampered, manifest, layout, 1); !errors.Is(err, ErrSnapshotIncompatible) {
		t.Errorf("tampered body: %v", err)
	}
	if _, err := decodeSnapshotBody(body[:len(body)-1], manifest, layout, 1); !errors.Is(err, ErrSnapshotIncompatible) {
		t.Errorf("truncated body: %v", err)
	}
	foreign := []sagaState{wireSaga(t, layout, 3)}
	foreignBody, _ := json.Marshal(foreign)
	fsum := sha256.Sum256(foreignBody)
	fm := good
	fm.Size, fm.SHA256 = len(foreignBody), hex.EncodeToString(fsum[:])
	if _, err := decodeSnapshotBody(foreignBody, fm, layout, 1); !errors.Is(err, ErrSnapshotIncompatible) {
		t.Errorf("saga of another partition in the body: %v", err)
	}
}

func TestLayoutSubjectsAndIDs(t *testing.T) {
	l := Layout{Prefix: "roost.saga", Stream: "ROOST_SAGA", Partitions: 5, Groups: 2}
	if got := l.StreamSubjects(1); strings.Join(got, ",") != "roost.saga.1.>,roost.saga.3.>" {
		t.Fatalf("stream subjects = %v", got)
	}
	if l.StreamOf(4) != "ROOST_SAGA_G0" || l.StreamOf(3) != "ROOST_SAGA_G1" {
		t.Fatalf("streams %s %s", l.StreamOf(4), l.StreamOf(3))
	}
	if l.CommandEventSubject(3, "gift.debit") != "roost.saga.3.ev.cmd.gift.debit" || l.CommandFilter("gift.debit") != "roost.saga.*.ev.cmd.gift.debit" {
		t.Fatal("command subjects")
	}
	if l.QuerySubject(2) != "roost.saga.q.2" || strings.HasPrefix(l.QuerySubject(2), "roost.saga.2.") {
		t.Fatal("query subject must stay outside the partition subjects")
	}
	if l.inputKindOf(3, l.ResultSubject(3)) != "res" || l.inputKindOf(3, l.ResultSubject(2)) != "" || !l.isEventSubject(3, l.DoneEventSubject(3)) {
		t.Fatal("subject classification")
	}
	id := IDOf("gift", "order-1")
	if len(id) != 32 || !validSubjectToken(id, 128) || id == IDOf("gift", "order-2") || id == IDOf("gift2", "order-1") {
		t.Fatalf("IDOf = %q", id)
	}
	if id != IDOf("gift", "order-1") {
		t.Fatal("IDOf is not stable")
	}
}
