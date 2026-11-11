package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

// log_wire.go 是 JetStream 协调器的线格式（WireVersion 3，设计 §4.2）：分区事件、三类输入、判定回复与查询、快照清单与正文。
// 一律 JSON；解码按版本精确匹配，不兼容更早的版本（线上未部署，升级需清空，见 CHANGELOG）。3 相对 2：记录多了必填的
// Lifecycle，操作键改为 "<ID>:<生命周期>:<方向>:<步骤>"（RV-20261030-01）。4 相对 3：路由步骤（记录、命令、启动意图与退回结果多了
// Route，启动摘要与回执摘要含 Route），路由步骤的命令事件改发 <prefix>.<k>.ev.to.<route>.<topic>（2026-11-11 按 sid 确切路由）。坏输入（解码失败、版本不对、
// 内容校验不过）由分区计数并跳过，输入水位照常前进（日志里的输入不能 Term）；坏事件与坏快照说明权威日志本身不可信，分区不启动。

// WireVersion 是 saga 全部线上消息的格式版本：分区事件、输入、快照清单、准入信号与 Nest 启动 effect。不兼容旧版本（1 是已删除
// 的 Redis 协调器与它的信封，2 是没有生命周期标识的记录与操作键，3 是没有路由键的记录与命令），解码按版本精确匹配。
const WireVersion uint16 = 4

// replyHeader 是启动 / 运维意图上的业务头：所有者把判定经核心 NATS 发到这个 subject（只给这一次尝试，选项 R-A）。
const replyHeader = "Roost-Saga-Reply"

// sagaState 是一笔 saga 在分区里的完整状态：记录、操作 tombstone 与已接收结果的回执摘要。每条事件携带它的新值，重放 = 按序覆盖。
// 提交之后不再修改（写时复制），快照可以直接引用已提交的值。
//
// 相对设计初稿多了 Receipts：按 CommandID 记回执摘要，用于“同一次尝试的重复送达”（重复）与“同一 CommandID
// 不同结论”（身份冲突）的判定（judgeHistory 的 Receipt 位、Apply 的重复检查）；分区要给出同样的结论，就得记同样的东西。
// 它随 saga 一起在终态保留期后剪掉，单笔条目数 = 这笔 saga 接收过的结果数。
type sagaState struct {
	Record   Record                 `json:"record"`
	Ops      map[string]opTombstone `json:"ops,omitempty"`
	Receipts map[string]string      `json:"receipts,omitempty"`
}

// opTombstone 是一个已关闭操作的记录：关闭方式（带结果 / 放弃，见 OperationClosure）与已告警的代际（放弃后迟到的成功按
// （操作，代际）只告警一次，B1）。
type opTombstone struct {
	Closure    OperationClosure `json:"closure"`
	LateAlarms []uint32         `json:"late_alarms,omitempty"`
}

// clone 复制出可以修改的新值（写时复制：已提交的值不动）。
func (s *sagaState) clone() *sagaState {
	c := &sagaState{Record: s.Record.Clone()}
	if len(s.Ops) > 0 {
		c.Ops = make(map[string]opTombstone, len(s.Ops))
		for op, tomb := range s.Ops {
			tomb.LateAlarms = slices.Clone(tomb.LateAlarms)
			c.Ops[op] = tomb
		}
	}
	if len(s.Receipts) > 0 {
		c.Receipts = maps.Clone(s.Receipts)
	}
	return c
}

// validate 校验一份解码出来的状态（事件、快照里的）：记录本身合法，tombstone 与回执都属于这笔 saga。
func (s *sagaState) validate() error {
	if err := s.Record.Validate(); err != nil {
		return err
	}
	for op, tomb := range s.Ops {
		if _, _, ok := parseOperationKey(s.Record, op); !ok || (tomb.Closure != OperationClosedWithResult && tomb.Closure != OperationAbandoned) {
			return fmt.Errorf("%w: saga %s tombstone %q", ErrInvalidRecord, s.Record.ID, op)
		}
	}
	for command, digest := range s.Receipts {
		if command == "" || len(digest) != 64 {
			return fmt.Errorf("%w: saga %s receipt %q", ErrInvalidRecord, s.Record.ID, command)
		}
	}
	return nil
}

// receiptDigest 是一份结果的稳定摘要：同一 CommandID 再次送达时，
// 摘要相同是重复，不同是身份冲突。
func receiptDigest(c Completion) string {
	stable := struct {
		Command   string `json:"command_id"`
		Key       string `json:"idempotency_key"`
		Saga      string `json:"saga_id"`
		Success   bool   `json:"success"`
		Retryable bool   `json:"retryable"`
		Data      []byte `json:"data"`
		Error     string `json:"error"`
		Bounced   bool   `json:"bounced,omitempty"`
		Route     string `json:"route,omitempty"`
	}{c.CommandID, c.IdempotencyKey, c.SagaID, c.Success, c.Retryable, c.Data, c.Error, c.Bounced, c.Route}
	raw, _ := json.Marshal(stable) // 只有字符串、布尔与字节切片，不会失败
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// 事件的 Kind：这一轮里这笔 saga 最后一次转移的原因（只用于观察与排障，重放不看它）。
var eventKinds = map[string]bool{
	"start": true, "dispatch": true, "result": true, "timeout": true, "deadline": true, "late_success": true,
	"late_alarm": true, "resume": true, "compensate": true, "definition_missing": true, "invalid_step": true,
}

func eventKindOf(cause transitionCause) string {
	switch cause {
	case causeDispatch:
		return "dispatch"
	case causeResult:
		return "result"
	case causeTimeout:
		return "timeout"
	case causeDeadline:
		return "deadline"
	case causeDefinitionMissing:
		return "definition_missing"
	case causeInvalidStep:
		return "invalid_step"
	case causeManualCompensate:
		return "compensate"
	case causeResume:
		return "resume"
	case causeLateSuccess:
		return "late_success"
	}
	return ""
}

// logEvent 是分区事件：一笔 saga 的完整新状态，派发时带要执行的一次尝试。
type logEvent struct {
	V      uint16 `json:"v"`
	Part   int    `json:"part"`
	Writer string `json:"writer"`
	// In 是写这条事件时分区的输入水位：序号 ≤ In 的输入已经处理过（重放从它之后取输入）。一轮切成几次发布时只有最后一次带
	// 这一轮的水位，之前的带上一轮的（中途崩溃时这一轮的输入会被重新处理，判定都是幂等的）。
	In      uint64    `json:"in"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Saga    sagaState `json:"saga"`
	Command *Command  `json:"command,omitempty"`
}

func encodeEvent(ev logEvent) ([]byte, error) {
	ev.V = WireVersion
	return json.Marshal(ev)
}

// decodeEvent 解码并校验分区 k 的一条事件。
func decodeEvent(raw []byte, layout Layout, k int) (logEvent, error) {
	var ev logEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return logEvent{}, fmt.Errorf("%w: event: %v", ErrInvalidRecord, err)
	}
	if ev.V != WireVersion {
		return logEvent{}, fmt.Errorf("%w: event wire version %d, want %d", ErrInvalidRecord, ev.V, WireVersion)
	}
	if ev.Part != k || !eventKinds[ev.Kind] || ev.Writer == "" {
		return logEvent{}, fmt.Errorf("%w: event part %d kind %q writer %q on partition %d", ErrInvalidRecord, ev.Part, ev.Kind, ev.Writer, k)
	}
	if err := ev.Saga.validate(); err != nil {
		return logEvent{}, err
	}
	if layout.PartitionOf(ev.Saga.Record.ID) != k {
		return logEvent{}, fmt.Errorf("%w: saga %s does not belong to partition %d", ErrInvalidRecord, ev.Saga.Record.ID, k)
	}
	if ev.Command != nil {
		if err := ev.Command.Validate(); err != nil || ev.Command.SagaID != ev.Saga.Record.ID || int(ev.Command.Partition) != k {
			return logEvent{}, fmt.Errorf("%w: event command for saga %s", ErrInvalidRecord, ev.Saga.Record.ID)
		}
	}
	return ev, nil
}

// startInput 是分区 in.start 上的启动意图。saga ID 不在里面：所有者按 IDOf(Type, BusinessKey) 派生（选项 ID-A）。
// Attempt 是这次启动尝试的全局唯一标识（RV-20261028-01），MsgID = start.<id>.<Attempt>；Digest 是发起方算的启动摘要，
// 所有者重算核对（不一致按坏输入处理）。
type startInput struct {
	V       uint16       `json:"v"`
	Request StartRequest `json:"request"`
	Attempt string       `json:"attempt"`
	Digest  string       `json:"digest"`
}

func decodeStartInput(raw []byte) (startInput, error) {
	var in startInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return startInput{}, fmt.Errorf("%w: start input: %v", ErrInvalidRecord, err)
	}
	if in.V != WireVersion || in.Attempt == "" || len(in.Attempt) > 256 || len(in.Digest) != 64 {
		return startInput{}, fmt.Errorf("%w: start input version %d attempt %q", ErrInvalidRecord, in.V, in.Attempt)
	}
	return in, nil
}

// resultInput 是分区 in.res 上的一份结果（MsgID res.<CommandID>）。
type resultInput struct {
	V          uint16     `json:"v"`
	Completion Completion `json:"completion"`
}

func decodeResultInput(raw []byte, maxPayload int) (Completion, error) {
	var in resultInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Completion{}, fmt.Errorf("%w: result input: %v", ErrInvalidRecord, err)
	}
	if in.V != WireVersion {
		return Completion{}, fmt.Errorf("%w: result input version %d", ErrInvalidRecord, in.V)
	}
	if err := in.Completion.Validate(); err != nil || len(in.Completion.Data) > maxPayload {
		return Completion{}, fmt.Errorf("%w: result input for command %q", ErrInvalidRecord, in.Completion.CommandID)
	}
	return in.Completion, nil
}

// 运维意图的种类。
const (
	opResume     = "resume"
	opCompensate = "compensate"
)

// opInput 是分区 in.op 上的运维意图（Resume / Compensate），与启动意图同样带全局唯一的尝试标识与回复头。
type opInput struct {
	V       uint16         `json:"v"`
	Kind    string         `json:"kind"`
	ID      string         `json:"id"`
	Resume  *ResumeRequest `json:"resume,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Now     time.Time      `json:"now"`
	Attempt string         `json:"attempt"`
}

func decodeOpInput(raw []byte) (opInput, error) {
	var in opInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return opInput{}, fmt.Errorf("%w: op input: %v", ErrInvalidRecord, err)
	}
	valid := in.V == WireVersion && in.Attempt != "" && len(in.Attempt) <= 256 && validSubjectToken(in.ID, 128) && !in.Now.IsZero() &&
		((in.Kind == opResume && in.Resume != nil && in.Resume.ID == in.ID) || (in.Kind == opCompensate && in.Resume == nil && len(in.Reason) <= 4096))
	if !valid {
		return opInput{}, fmt.Errorf("%w: op input version %d kind %q", ErrInvalidRecord, in.V, in.Kind)
	}
	return in, nil
}

// 判定（verdict）是所有者对一次启动 / 运维尝试的回答，只发给这次尝试的回复 subject（选项 R-A：拒绝不写日志、不唤醒别人）。
const (
	verdictAccepted          = "accepted"           // 写了事件：新 saga 已建立 / 运维操作已生效
	verdictExisting          = "existing"           // 同一意图（启动摘要相同）已经存在，返回当前记录，不写事件
	verdictIdentityConflict  = "identity_conflict"  // 同一业务键、不同意图（ErrIdentityConflict）
	verdictRejected          = "rejected"           // 准入关闭（ErrBackpressure，Reason 是原因）
	verdictDefinitionMissing = "definition_missing" // 所有者没有这个定义版本（ErrDefinitionMissing）
	verdictNotFound          = "not_found"          // 运维意图找不到记录（ErrNotFound）
	verdictInvalid           = "invalid"            // 输入或请求不合法（ErrInvalidRecord / 判定给出的错误，Error 是文本）
	verdictSagaCompleted     = "saga_completed"     // Completed 的 saga 不能 Compensate（ErrSagaCompleted，带记录）
	verdictDeadlineExpired   = "deadline_expired"   // ErrDeadlineExpired
	verdictReceiptsExpired   = "receipts_expired"   // ErrReceiptsExpired
	verdictUnchanged         = "unchanged"          // Compensate：已在补偿或已补偿，原样返回记录
	verdictRefused           = "refused"            // 规则拒绝（如状态不能 Resume、补偿时有结果在途），Error 是原文
)

// verdictReply 是判定在核心 NATS 上的形状。
type verdictReply struct {
	V       uint16  `json:"v"`
	Attempt string  `json:"attempt"`
	Outcome string  `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	Error   string  `json:"error,omitempty"`
	Record  *Record `json:"record,omitempty"`
}

// queryRequest / queryReply 是向分区所有者问记录（Get / List）的核心 NATS 请求与回复；列表散播时每个分区回一条。
type queryRequest struct {
	V     uint16 `json:"v"`
	Kind  string `json:"kind"` // get / list
	ID    string `json:"id,omitempty"`
	Query *Query `json:"query,omitempty"`
	// Known 是 list 时发起方从冷副本读到的 ID：所有者在 Held 里回答其中哪些在自己内存里（这些 ID 以内存为准，冷副本那条不用）。
	Known []string `json:"known,omitempty"`
}

type queryReply struct {
	V       uint16   `json:"v"`
	Part    int      `json:"part"`
	Found   bool     `json:"found"`
	Record  *Record  `json:"record,omitempty"`
	Records []Record `json:"records,omitempty"`
	// Held 是 Known 里在本分区内存里的 ID（不论是否匹配查询条件）。
	Held  []string `json:"held,omitempty"`
	Error string   `json:"error,omitempty"`
}

// snapshotWireVersion 是快照清单与正文的版本（与 WireVersion 同步）。
const snapshotWireVersion = WireVersion

// snapshotLayout 是快照清单里记的布局（加载时与本进程比对）。
type snapshotLayout struct {
	Partitions int    `json:"partitions"`
	Groups     int    `json:"groups"`
	Prefix     string `json:"prefix"`
	Stream     string `json:"stream"`
}

func layoutOf(l Layout) snapshotLayout {
	return snapshotLayout{Partitions: l.Partitions, Groups: l.Groups, Prefix: l.Prefix, Stream: l.Stream}
}

// snapshotManifest 是分区快照的清单（KV 键 p<k>）：正文在对象桶里（KV 值受 max_payload 限制）。
//
// LastEv 是快照覆盖到的最后一条事件，InHi 是快照时的输入水位：重放从 min(InHi, LastEv)+1 读起，事件只取 LastEv 之后的，
// 输入只取 InHi 之后的（快照是一轮结束时的已提交状态：序号 ≤ LastEv 的事件都已在里面，序号 ≤ InHi 的输入都已处理过）。
type snapshotManifest struct {
	V      uint16         `json:"v"`
	Part   int            `json:"part"`
	Layout snapshotLayout `json:"layout"`
	LastEv uint64         `json:"last_ev"`
	InHi   uint64         `json:"in_hi"`
	Writer string         `json:"writer"`
	At     time.Time      `json:"at"`
	Object string         `json:"object"`
	Size   int            `json:"size"`
	SHA256 string         `json:"sha256"`
}

// decodeManifest 解码分区 k 的快照清单并校验版本、分区与布局；任何一项不符返回 ErrSnapshotIncompatible。
func decodeManifest(raw []byte, layout Layout, k int) (snapshotManifest, error) {
	var m snapshotManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return snapshotManifest{}, fmt.Errorf("%w: partition %d manifest: %v", ErrSnapshotIncompatible, k, err)
	}
	switch {
	case m.V != snapshotWireVersion:
		return snapshotManifest{}, fmt.Errorf("%w: partition %d manifest version %d, want %d", ErrSnapshotIncompatible, k, m.V, snapshotWireVersion)
	case m.Part != k:
		return snapshotManifest{}, fmt.Errorf("%w: manifest of partition %d stored under partition %d", ErrSnapshotIncompatible, m.Part, k)
	case m.Layout != layoutOf(layout):
		return snapshotManifest{}, fmt.Errorf("%w: partition %d manifest layout %+v, this process %+v", ErrSnapshotIncompatible, k, m.Layout, layoutOf(layout))
	case m.Object == "" || m.Size < 0 || len(m.SHA256) != 64 || m.LastEv == 0:
		return snapshotManifest{}, fmt.Errorf("%w: partition %d manifest object %q size %d", ErrSnapshotIncompatible, k, m.Object, m.Size)
	}
	return m, nil
}

// decodeSnapshotBody 校验快照正文的长度与 SHA-256（与清单一致），再解码并逐笔校验状态与分区归属。
func decodeSnapshotBody(raw []byte, m snapshotManifest, layout Layout, k int) ([]sagaState, error) {
	sum := sha256.Sum256(raw)
	if len(raw) != m.Size || hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, fmt.Errorf("%w: partition %d snapshot body size %d / digest differ from the manifest", ErrSnapshotIncompatible, k, len(raw))
	}
	var states []sagaState
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, fmt.Errorf("%w: partition %d snapshot body: %v", ErrSnapshotIncompatible, k, err)
	}
	for i := range states {
		if err := states[i].validate(); err != nil || layout.PartitionOf(states[i].Record.ID) != k {
			return nil, fmt.Errorf("%w: partition %d snapshot saga %q", ErrSnapshotIncompatible, k, states[i].Record.ID)
		}
	}
	return states, nil
}

// layoutRecord 是快照桶 layout 键里登记的布局。
type layoutRecord struct {
	V      uint16         `json:"v"`
	Layout snapshotLayout `json:"layout"`
}

// participantLoad 是参与方发布的准入信号（LoadBucket 的 part.<service>.<sid>）：DataEngine 投影器的 WALUnacked（记录数）。
// 过期按协调器本机单调时钟判定（最后一次收到更新的时刻），不读这里的时间。
type participantLoad struct {
	V       uint16 `json:"v"`
	Records uint64 `json:"records"`
}

// admissionHint 是所有者发布的准入状态（LoadBucket 的 admit 键），只是提示：StartSaga 据它快速失败，权威判定在所有者处理
// 启动意图时。
type admissionHint struct {
	V              uint16 `json:"v"`
	Open           bool   `json:"open"`
	Reason         string `json:"reason,omitempty"`
	BacklogRecords uint64 `json:"backlog_records"`
}
