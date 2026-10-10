package syncstream

import (
	"errors"
	"testing"
)

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
