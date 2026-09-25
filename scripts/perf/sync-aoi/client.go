package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	packetFrame   uint32 = 1
	packetBarrier uint32 = 2
	packetFinish  uint32 = 3
)

func readFull(r io.Reader, p []byte) (int, error) { return io.ReadFull(r, p) }
func writePacket(conn net.Conn, kind uint32, tick int, planned int64, payload []byte) error {
	if kind != packetFrame {
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
	}
	var h [24]byte
	binary.LittleEndian.PutUint32(h[:4], kind)
	binary.LittleEndian.PutUint32(h[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint64(h[8:16], uint64(tick))
	binary.LittleEndian.PutUint64(h[16:24], uint64(planned))
	buffers := net.Buffers{h[:], payload}
	_, err := buffers.WriteTo(conn)
	return err
}

type objectState struct {
	ID      int64
	Version uint64
}
type clientResult struct {
	frames, bytes, updates, creates, removes int64
	planned, changed, committed, dispatch    []float64
	peak, final                              int
	err                                      error
}
type clientReport struct {
	Frames       int64              `json:"frames"`
	Bytes        int64              `json:"sync_bytes"`
	Samples      int64              `json:"mutation_samples"`
	Planned      map[string]float64 `json:"planned_event_to_client_ms"`
	Changed      map[string]float64 `json:"state_change_to_client_ms"`
	Committed    map[string]float64 `json:"commit_to_client_ms"`
	Dispatch     map[string]float64 `json:"transport_admission_to_client_ms"`
	PlannedOver  int64              `json:"planned_event_over_50ms"`
	ChangedOver  int64              `json:"state_change_over_50ms"`
	Creates      int64              `json:"creates"`
	Removes      int64              `json:"removes"`
	Updates      int64              `json:"decoded_updates"`
	VisibleFinal map[string]float64 `json:"visible_final"`
	VisiblePeak  map[string]float64 `json:"visible_peak"`
	AllocBytes   uint64             `json:"client_alloc_bytes"`
	GCCycles     uint32             `json:"client_gc_cycles"`
	GCPauseMS    float64            `json:"client_gc_pause_ms"`
	GOMAXPROCS   int                `json:"client_gomaxprocs"`
	Errors       []string           `json:"errors,omitempty"`
}

func runClient(c config) error {
	results := make(chan clientResult, c.Players)
	var wg sync.WaitGroup
	var ready atomic.Int64
	var before runtime.MemStats
	for _, id := range observerIDs(c) {
		conn, err := net.DialTimeout("tcp", c.Address, 10*time.Second)
		if err != nil {
			return err
		}
		var h [8]byte
		binary.LittleEndian.PutUint64(h[:], uint64(id))
		if _, err := conn.Write(h[:]); err != nil {
			_ = conn.Close()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			result := consume(conn, c, func() {
				if ready.Add(1) == int64(c.Players) {
					runtime.ReadMemStats(&before)
				}
			})
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	report := clientReport{GOMAXPROCS: runtime.GOMAXPROCS(0), AllocBytes: after.TotalAlloc - before.TotalAlloc, GCCycles: after.NumGC - before.NumGC, GCPauseMS: float64(after.PauseTotalNs-before.PauseTotalNs) / 1e6}
	var planned, changed, committed, dispatch, visible, peak []float64
	for r := range results {
		if r.err != nil {
			report.Errors = append(report.Errors, r.err.Error())
		}
		report.Frames += r.frames
		report.Bytes += r.bytes
		report.Creates += r.creates
		report.Removes += r.removes
		report.Updates += r.updates
		planned = append(planned, r.planned...)
		changed = append(changed, r.changed...)
		committed = append(committed, r.committed...)
		dispatch = append(dispatch, r.dispatch...)
		visible = append(visible, float64(r.final))
		peak = append(peak, float64(r.peak))
	}
	report.Samples = int64(len(planned))
	for _, v := range planned {
		if v > 50 {
			report.PlannedOver++
		}
	}
	for _, v := range changed {
		if v > 50 {
			report.ChangedOver++
		}
	}
	report.Planned, report.Changed, report.Dispatch = quantiles(planned), quantiles(changed), quantiles(dispatch)
	report.Committed = quantiles(committed)
	report.VisibleFinal, report.VisiblePeak = quantiles(visible), quantiles(peak)
	if err := writeJSON(filepath.Join(c.Output, "client.json"), report); err != nil {
		return err
	}
	if len(report.Errors) > 0 {
		return fmt.Errorf("%d client errors; first: %s", len(report.Errors), report.Errors[0])
	}
	return nil
}

// 每条连接独立解码、维护 ObjectRef 和版本，避免同进程全局校验锁影响服务端。
// 最后与 Interest 的真实可见集合逐项比较，不能只用“平均 50 个”掩盖漏发。
func consume(conn net.Conn, c config, onBarrier func()) (r clientResult) {
	refs := make(map[frame.ObjectRef]objectState)
	measured := make(map[int64]int64)
	var epoch, clock uint32
	for {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		var h [24]byte
		if _, err := readFull(conn, h[:]); err != nil {
			r.err = err
			return
		}
		kind, size := binary.LittleEndian.Uint32(h[:4]), binary.LittleEndian.Uint32(h[4:8])
		tick := int(int64(binary.LittleEndian.Uint64(h[8:16])))
		planned := int64(binary.LittleEndian.Uint64(h[16:]))
		if size > 4<<20 {
			r.err = fmt.Errorf("oversize frame %d", size)
			return
		}
		raw := make([]byte, int(size))
		if _, err := readFull(conn, raw); err != nil {
			r.err = err
			return
		}
		switch kind {
		case packetBarrier:
			r = clientResult{peak: len(refs)}
			onBarrier()
			if _, err := conn.Write([]byte{1}); err != nil {
				r.err = err
				return
			}
			continue
		case packetFinish:
			var expected []int64
			if err := json.Unmarshal(raw, &expected); err != nil {
				r.err = err
				return
			}
			actual := make([]int64, 0, len(refs))
			for _, s := range refs {
				actual = append(actual, s.ID)
			}
			slices.Sort(actual)
			slices.Sort(expected)
			if !slices.Equal(actual, expected) {
				r.err = fmt.Errorf("final AOI mismatch: actual %v expected %v", actual, expected)
			}
			r.final = len(refs)
			return
		case packetFrame:
		default:
			r.err = fmt.Errorf("unexpected packet kind %d", kind)
			return
		}
		decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
		if err != nil {
			r.err = err
			return
		}
		if decoded.Epoch != epoch {
			clear(refs)
			epoch = decoded.Epoch
			clock = 0
		}
		if decoded.BaseTick != clock {
			r.err = fmt.Errorf("frame chain: base=%d client=%d", decoded.BaseTick, clock)
			return
		}
		clock = decoded.Tick
		for _, object := range decoded.Objects {
			current, exists := refs[object.Ref]
			switch object.Operation {
			case frame.ObjectRemove:
				if !exists {
					r.err = fmt.Errorf("remove of unknown object %+v", object.Ref)
					return
				}
				delete(refs, object.Ref)
				if tick > 0 {
					r.removes++
				}
				continue
			case frame.ObjectCreate:
				if exists {
					r.err = fmt.Errorf("duplicate create %+v", object.Ref)
					return
				}
				if tick > 0 {
					r.creates++
				}
			case frame.ObjectUpdate:
				if !exists {
					r.err = fmt.Errorf("update of unknown object %+v", object.Ref)
					return
				}
			default:
				r.err = fmt.Errorf("invalid operation %v", object.Operation)
				return
			}
			if len(object.Components) != 1 {
				r.err = fmt.Errorf("unexpected component count %d", len(object.Components))
				return
			}
			update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
			if err != nil {
				r.err = err
				return
			}
			if exists && current.ID != update.SubjectID {
				r.err = fmt.Errorf("reference subject changed without create")
				return
			}
			if !update.Full && (!exists || current.Version != update.BaseVersion) {
				r.err = fmt.Errorf("subject %d version gap: base=%d client=%d", update.SubjectID, update.BaseVersion, current.Version)
				return
			}
			var value component
			if err := bson.Unmarshal(update.Payload.BytesCopy(), &value); err != nil {
				r.err = err
				return
			}
			at := position(value.ID, int(value.Step), c)
			hp := int64(100)
			if value.Changed != 0 {
				hp -= int64((int(value.Step) + 10000) % 97)
			}
			if value.ID != update.SubjectID || workloadIndex(value.ID, c) < 1 || workloadIndex(value.ID, c) > int64(c.Entities) || value.X != at.X || value.Y != at.Y || value.HP != hp || len(value.Extra) != c.Padding {
				r.err = fmt.Errorf("component mismatch for %d step %d", value.ID, value.Step)
				return
			}
			for _, v := range value.Extra {
				if v != byte(value.ID%251) {
					r.err = fmt.Errorf("payload corruption for %d", value.ID)
					return
				}
			}
			refs[object.Ref] = objectState{ID: value.ID, Version: update.Version}
			if tick > 0 {
				r.updates++
				// Old snapshots are not new state changes. Only this tick's changed
				// subjects contribute to the mutation SLO; all frames are still verified.
				if value.Step > 0 && value.Step > measured[value.ID] && (value.Step == int64(tick) || object.Operation == frame.ObjectUpdate) {
					measured[value.ID] = value.Step
					now := time.Now().UnixNano()
					r.planned = append(r.planned, float64(now-value.Planned)/1e6)
					r.changed = append(r.changed, float64(now-value.Changed)/1e6)
					if value.Committed > 0 {
						r.committed = append(r.committed, float64(now-value.Committed)/1e6)
					}
				}
			}
		}
		r.peak = max(r.peak, len(refs))
		if tick > 0 {
			r.frames++
			r.bytes += int64(size)
			r.dispatch = append(r.dispatch, float64(time.Now().UnixNano()-planned)/1e6)
		}
	}
}
