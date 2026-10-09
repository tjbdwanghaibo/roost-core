package entitysync

import (
	"fmt"
	"sync"
	"time"
)

// SyncTraceEvent 是可关联的诊断事实，不保存 payload 或业务对象。
// SubjectID/Version 关联内容，Session/Lifetime/Epoch/Tick 关联交付；零值表示该阶段尚未赋值。
// Duration 是本阶段耗时；At 是阶段结束的 Unix 纳秒，仅供同机诊断。
type SyncTraceEvent struct {
	At                              int64
	Stage                           string
	SubjectID                       int64
	Session                         SessionID
	Lifetime                        uint64
	Epoch, Tick                     uint32
	Version, BaseVersion, CommitLSN uint64
	Full, Snapshot                  bool
	Reason                          uint8
	Duration                        time.Duration
}

// SyncTrace 使用固定容量环形记录。默认不配置；诊断方可定期 Drain 写文件，
// 覆盖数与记录一并返回，不能将已覆盖的阶段解释为没有发生。
type SyncTrace struct {
	mu          sync.Mutex
	events      []SyncTraceEvent
	next, count int
	overwritten uint64
}

func NewSyncTrace(capacity int) (*SyncTrace, error) {
	if capacity < 1 || capacity > 1<<20 {
		return nil, fmt.Errorf("entitysync: trace capacity must be 1..1048576")
	}
	return &SyncTrace{events: make([]SyncTraceEvent, capacity)}, nil
}

// Record 可由传输装配方补充发送阶段，不能传入 payload 或动态构建的无限长 Stage。
func (t *SyncTrace) Record(event SyncTraceEvent) {
	if t == nil {
		return
	}
	if len(event.Stage) > 64 {
		return
	}
	if event.At == 0 {
		event.At = time.Now().UnixNano()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) == 0 {
		return
	}
	if t.count == len(t.events) {
		t.overwritten++
	} else {
		t.count++
	}
	t.events[t.next] = event
	t.next = (t.next + 1) % len(t.events)
}

func (m *Manager) traceSnapshotRequest(subj *subject, sess *session) {
	if m.config.Trace != nil {
		m.config.Trace.Record(SyncTraceEvent{Stage: "snapshot_requested", SubjectID: subj.id, Session: sess.id, Lifetime: sess.lifetime.traceID, Epoch: sess.epoch, Snapshot: true})
		if _, exists := sess.objects[subj.id]; !exists {
			// Snapshot 在该事件中表示 reset 时已存在的恢复集合成员。
			m.config.Trace.Record(SyncTraceEvent{Stage: "baseline_requested", SubjectID: subj.id, Session: sess.id, Lifetime: sess.lifetime.traceID, Epoch: sess.epoch, Snapshot: sess.held})
		}
	}
}

// Drain 返回独立副本并清空已取记录；写盘不占诊断锁。
func (t *SyncTrace) Drain() ([]SyncTraceEvent, uint64) {
	if t == nil {
		return nil, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SyncTraceEvent, t.count)
	for i := range out {
		out[i] = t.events[(t.next-t.count+i+len(t.events))%len(t.events)]
	}
	dropped := t.overwritten
	clear(t.events)
	t.next, t.count, t.overwritten = 0, 0, 0
	return out, dropped
}
