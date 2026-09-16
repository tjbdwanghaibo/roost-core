package syncstream

import (
	"errors"
	"testing"
)

// U-0214 · C8 · RR-20260915-09:Recover 在锁外调用 provider,返回后不比较开始时的 epoch / 流状态,
// 直接 Append——provider 捕获期间同流有了新的 Full,或先 RotateEpoch 再发新包,旧捕获被标成更高
// 的序号 / 新 epoch 提交,接收者按更高序号应用 Full 时回退到旧内容。承诺:调用 provider 前记下
// epoch 与目标流的 latest,返回后在同一互斥边界下核对,不一致则不提交、返回可重试的 ErrRecoverStale。
func TestRecoverPromiseDoesNotCommitAStaleCapture(t *testing.T) {
	for _, mode := range []string{"no_change", "same_stream_append", "epoch_rotation", "provider_error", "replay_skips_provider"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHistory(HistoryOptions{Epoch: 7})
			s := Stream{Topic: "state"}
			if mode == "replay_skips_provider" {
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("current")})
				r, err := h.Recover(ResyncRequest{Stream: s, Epoch: 7}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
					t.Fatal("provider called for replay")
					return Packet{}, nil
				}))
				if err != nil || len(r.Packets) != 1 {
					t.Fatal(err)
				}
				return
			}
			entered, release := make(chan struct{}), make(chan struct{})
			type outcome struct {
				r ResyncResult
				e error
			}
			done := make(chan outcome, 1)
			go func() {
				r, e := h.Recover(ResyncRequest{Stream: s, Epoch: 7}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
					captured := Packet{Payload: []byte("old")}
					close(entered)
					<-release
					if mode == "provider_error" {
						return Packet{}, errors.New("capture failed")
					}
					return captured, nil
				}))
				done <- outcome{r, e}
			}()
			<-entered
			if mode == "same_stream_append" {
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("new")})
			}
			if mode == "epoch_rotation" {
				h.RotateEpoch(8)
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("new")})
			}
			close(release)
			got := <-done
			switch mode {
			case "provider_error":
				if got.e == nil || h.Metrics().Streams != 0 {
					t.Fatal("provider failure mutated history")
				}
			case "no_change":
				if got.e != nil || len(got.r.Packets) != 1 || string(got.r.Packets[0].Payload) != "old" {
					t.Fatalf("plain recover: %+v %v", got.r, got.e)
				}
			default:
				if got.e == nil {
					p := got.r.Packets[0]
					t.Fatalf("stale capture appended after newer state: epoch=%d seq=%d payload=%q", p.Epoch, p.Sequence, p.Payload)
				}
				status := h.Status(Observer{}, s)
				if string(h.Export().Streams[0].Packets[len(h.Export().Streams[0].Packets)-1].Payload) != "new" || status.LatestSequence != 1 {
					t.Fatalf("stale recover disturbed the newer state: %+v", status)
				}
			}
		})
	}
}
