package skill

import (
	"fmt"
	"sync"
)

// ErrReplayMismatch reports a changed, missing, or reordered Host interaction
// while replaying a recorded deterministic run.
var ErrReplayMismatch = fmt.Errorf("skill: replay host mismatch")

type HostRecord struct {
	Kind           string
	Request        string
	Result         any
	Err            error
	BeforeRevision WorldRevision
	AfterRevision  WorldRevision
}

// RecordingHost preserves typed host results for deterministic test replay.
// It is a debugging adapter and is intentionally not a production world model.
type RecordingHost struct {
	host    Host
	mutex   sync.Mutex
	records []HostRecord
}

func NewRecordingHost(host Host) *RecordingHost { return &RecordingHost{host: host} }

func (host *RecordingHost) Records() []HostRecord {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	return append([]HostRecord(nil), host.records...)
}

func (host *RecordingHost) append(kind string, request, result any, err error, before WorldRevision) {
	host.mutex.Lock()
	host.records = append(host.records, HostRecord{Kind: kind, Request: hostRequestKey(request), Result: result, Err: err, BeforeRevision: before, AfterRevision: host.host.CurrentRevision()})
	host.mutex.Unlock()
}

func hostRequestKey(value any) string { return fmt.Sprintf("%T:%#v", value, value) }
func (host *RecordingHost) AuthorityIdentity() AuthorityIdentity {
	value := host.host.AuthorityIdentity()
	host.append("authority", nil, value, nil, host.host.CurrentRevision())
	return value
}
func (host *RecordingHost) CurrentRevision() WorldRevision {
	value := host.host.CurrentRevision()
	host.append("current_revision", nil, value, nil, value)
	return value
}
func (host *RecordingHost) Advance(value Tick) (WorldRevision, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.Advance(value)
	host.append("advance", value, result, err, before)
	return result, err
}
func (host *RecordingHost) Read(value ReadRequest) (ReadResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.Read(value)
	host.append("read", value, result, err, before)
	return result, err
}
func (host *RecordingHost) Select(value SelectRequest) (SelectResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.Select(value)
	host.append("select", value, result, err, before)
	return result, err
}
func (host *RecordingHost) PayCosts(value CostPayment) (CommitReceipt, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.PayCosts(value)
	host.append("pay_costs", value, result, err, before)
	return result, err
}
func (host *RecordingHost) Apply(value EffectCommand) (EffectResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.Apply(value)
	host.append("apply", value, result, err, before)
	return result, err
}
func (host *RecordingHost) StepProcess(value ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.StepProcess(value, state)
	host.append("step_process", struct {
		Command ProcessStepCommand
		State   ProcessHostState
	}{value, state}, result, err, before)
	return result, err
}
func (host *RecordingHost) StopProcess(value ProcessStopCommand, state ProcessHostState) (CommitReceipt, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.StopProcess(value, state)
	host.append("stop_process", struct {
		Command ProcessStopCommand
		State   ProcessHostState
	}{value, state}, result, err, before)
	return result, err
}
func (host *RecordingHost) Events(value EventCursor) []RuntimeEvent {
	result := host.host.Events(value)
	host.append("events", value, result, nil, host.host.CurrentRevision())
	return result
}
func (host *RecordingHost) ReadState(value StateReadRequest) (StateReadResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.ReadState(value)
	host.append("read_state", value, result, err, before)
	return result, err
}
func (host *RecordingHost) ModifyState(value StateMutationCommand) (StateMutationResult, error) {
	before := host.host.CurrentRevision()
	result, err := host.host.ModifyState(value)
	host.append("modify_state", value, result, err, before)
	return result, err
}

// ReplayHost returns the captured typed result only when the next interaction
// has the same kind and canonical debug representation as the recorded call.
type ReplayHost struct {
	authority AuthorityIdentity
	records   []HostRecord
	index     int
}

func NewReplayHost(authority AuthorityIdentity, records []HostRecord) *ReplayHost {
	return &ReplayHost{authority: authority, records: append([]HostRecord(nil), records...)}
}
func (host *ReplayHost) AuthorityIdentity() AuthorityIdentity {
	return host.next("authority", nil).(AuthorityIdentity)
}
func (host *ReplayHost) CurrentRevision() WorldRevision {
	return host.next("current_revision", nil).(WorldRevision)
}
func (host *ReplayHost) Advance(value Tick) (WorldRevision, error) {
	record := host.nextRecord("advance", value)
	return record.Result.(WorldRevision), record.Err
}
func (host *ReplayHost) Read(value ReadRequest) (ReadResult, error) {
	record := host.nextRecord("read", value)
	return record.Result.(ReadResult), record.Err
}
func (host *ReplayHost) Select(value SelectRequest) (SelectResult, error) {
	record := host.nextRecord("select", value)
	return record.Result.(SelectResult), record.Err
}
func (host *ReplayHost) PayCosts(value CostPayment) (CommitReceipt, error) {
	record := host.nextRecord("pay_costs", value)
	return record.Result.(CommitReceipt), record.Err
}
func (host *ReplayHost) Apply(value EffectCommand) (EffectResult, error) {
	record := host.nextRecord("apply", value)
	return record.Result.(EffectResult), record.Err
}
func (host *ReplayHost) StepProcess(value ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	record := host.nextRecord("step_process", struct {
		Command ProcessStepCommand
		State   ProcessHostState
	}{value, state})
	return record.Result.(ProcessStepResult), record.Err
}
func (host *ReplayHost) StopProcess(value ProcessStopCommand, state ProcessHostState) (CommitReceipt, error) {
	record := host.nextRecord("stop_process", struct {
		Command ProcessStopCommand
		State   ProcessHostState
	}{value, state})
	return record.Result.(CommitReceipt), record.Err
}
func (host *ReplayHost) Events(value EventCursor) []RuntimeEvent {
	return host.next("events", value).([]RuntimeEvent)
}
func (host *ReplayHost) ReadState(value StateReadRequest) (StateReadResult, error) {
	record := host.nextRecord("read_state", value)
	return record.Result.(StateReadResult), record.Err
}
func (host *ReplayHost) ModifyState(value StateMutationCommand) (StateMutationResult, error) {
	record := host.nextRecord("modify_state", value)
	return record.Result.(StateMutationResult), record.Err
}
func (host *ReplayHost) AssertComplete() error {
	if host.index != len(host.records) {
		return fmt.Errorf("%w: %d unconsumed calls", ErrReplayMismatch, len(host.records)-host.index)
	}
	return nil
}
func (host *ReplayHost) next(kind string, request any) any {
	return host.nextRecord(kind, request).Result
}
func (host *ReplayHost) nextRecord(kind string, request any) HostRecord {
	if host.index >= len(host.records) {
		panic(fmt.Errorf("%w: unexpected %s", ErrReplayMismatch, kind))
	}
	record := host.records[host.index]
	host.index++
	if record.Kind != kind || record.Request != hostRequestKey(request) {
		panic(fmt.Errorf("%w: got %s %s, want %s %s", ErrReplayMismatch, kind, hostRequestKey(request), record.Kind, record.Request))
	}
	return record
}
