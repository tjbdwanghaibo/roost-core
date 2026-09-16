package syncstream

import (
	"errors"
	"testing"
)

// U-0216 · C8 · RR-20260916-04:Recover 的一致性核对只比较"当前位置"(epoch / 流是否存在 / latest),
// 位置相同不等于中间没发生过替换。两种交错都能确定性复现:流原本不存在,provider 捕获期间有人 Append 又
// DeleteStream,提交时仍是"不存在、同 epoch",旧捕获被当成 seq=2 的 Full 追加;流原本 latest=1,期间 Import 了
// 同 epoch / 同 latest 但内容不同的快照,旧捕获作为 seq=2 覆盖新内容。承诺:History 维护一个进程内单调的修改代数,
// 任何成功改变流集合 / epoch / 序号地板 / ACK 的操作都推进它;Recover 在调 provider 前捕获代数,写锁内代数不同就
// 不提交、返回 ErrRecoverStale。位置比较不再是判据——它推不出"期间无事发生"。
func TestRecoverPromiseRejectsReplacementAtTheSamePosition(t *testing.T) {
	for _, mode := range []string{"no_change", "append_control", "append_delete_absent", "same_position_import", "delete_observer_absent"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHistory(HistoryOptions{Epoch: 7})
			s := Stream{Topic: "state"}
			appendFull := func(x *History, payload string) {
				t.Helper()
				if _, err := x.Append(Packet{Stream: s, Full: true, Payload: []byte(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "same_position_import" {
				appendFull(h, "before")
			}
			result, err := h.Recover(ResyncRequest{Stream: s, Epoch: 999}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
				// Everything below happens after the capture began and before
				// it is committed: the interleaving the position check misses.
				switch mode {
				case "append_delete_absent":
					appendFull(h, "new")
					if e := h.DeleteStream(Observer{}, s); e != nil {
						t.Fatal(e)
					}
				case "delete_observer_absent":
					appendFull(h, "new")
					if _, e := h.DeleteObserver(Observer{}); e != nil {
						t.Fatal(e)
					}
				case "same_position_import":
					fresh := NewHistory(HistoryOptions{Epoch: 7})
					appendFull(fresh, "replacement")
					if e := h.Import(fresh.Export()); e != nil {
						t.Fatal(e)
					}
				case "append_control":
					appendFull(h, "new")
				}
				return Packet{Payload: []byte("stale")}, nil
			}))
			if mode == "no_change" {
				if err != nil || len(result.Packets) != 1 || string(result.Packets[0].Payload) != "stale" {
					t.Fatalf("plain recover: %+v %v", result, err)
				}
				return
			}
			if !errors.Is(err, ErrRecoverStale) {
				payload := ""
				if len(result.Packets) > 0 {
					payload = string(result.Packets[0].Payload)
				}
				t.Fatalf("stale capture accepted after %s: err=%v committed payload=%q", mode, err, payload)
			}
			// The newer state is untouched: whatever the interleaving left is
			// what a subsequent reader sees, never the stale capture.
			for _, item := range h.Export().Streams {
				for _, p := range item.Packets {
					if string(p.Payload) == "stale" {
						t.Fatalf("stale capture is in the history after %s", mode)
					}
				}
			}
		})
	}
}
