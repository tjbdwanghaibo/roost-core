package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// 诊断运行独立于性能门禁运行；有界内存逐批写 JSONL，不累计整轮事件。
func openSyncTrace(c config) (*entitysync.SyncTrace, func() error, func(), error) {
	noop := func() error { return nil }
	if c.TraceCapacity == 0 {
		return nil, noop, func() {}, nil
	}
	trace, err := entitysync.NewSyncTrace(c.TraceCapacity)
	if err != nil {
		return nil, noop, func() {}, err
	}
	f, err := os.Create(filepath.Join(c.Output, "trace.jsonl"))
	if err != nil {
		return nil, noop, func() {}, err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	encoder := json.NewEncoder(w)
	drain := func() error {
		events, overwritten := trace.Drain()
		if overwritten != 0 {
			if err := encoder.Encode(map[string]any{"overwritten": overwritten}); err != nil {
				return err
			}
		}
		for _, event := range events {
			if err := encoder.Encode(event); err != nil {
				return err
			}
		}
		return w.Flush()
	}
	return trace, drain, func() { _ = w.Flush(); _ = f.Close() }, nil
}

func (l *serverTransport) traceSent(sid entitysync.SessionID, raw []byte, started time.Time) {
	if l.trace == nil {
		return
	}
	at := time.Now()
	f, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
	if err != nil {
		return
	}
	l.trace.Record(entitysync.SyncTraceEvent{At: at.UnixNano(), Stage: "sent", Session: sid, Epoch: f.Epoch, Tick: f.Tick, Duration: at.Sub(started)})
}

type latencyOutlier struct {
	Session, SubjectID                              int64
	Epoch, Tick                                     uint32
	Version, BaseVersion                            uint64
	Step, EnvelopeStep                              int64
	Full                                            bool
	Reason                                          uint8
	Operation                                       uint8
	Planned, Changed, Committed, Admitted, Received int64
}

// 只在诊断运行记录实际解码成功的 Create/Remove；与服务端准入时间分开。
// 每连接最多保留 4096 条、整轮文件最多输出 1<<20 条；省略时离线报告不能宣称恢复完整。
type baselineReceipt struct {
	Session, SubjectID int64
	Epoch              uint32
	At                 int64
	Operation          uint8
}

type baselineReceipts struct {
	Receipts []baselineReceipt `json:"receipts"`
	Omitted  int64             `json:"omitted"`
}

func (r *clientResult) recordBaseline(sessionID []int64, subject int64, epoch uint32, operation frame.ObjectOperation) {
	if len(r.baselineReceipts) >= 4096 {
		r.baselineOmitted++
		return
	}
	var sid int64
	if len(sessionID) > 0 {
		sid = sessionID[0]
	}
	r.baselineReceipts = append(r.baselineReceipts, baselineReceipt{Session: sid, SubjectID: subject, Epoch: epoch, Operation: uint8(operation), At: time.Now().UnixNano()})
}
