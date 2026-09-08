package skill

import "fmt"

func (host *MemoryHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return ProcessStepResult{}, err
	}
	processID := command.Meta.ProcessID
	if processID == 0 {
		processID = state.ProcessID
	}
	if processID == 0 {
		return ProcessStepResult{}, fmt.Errorf("skill: process id is required")
	}
	state.ProcessID = processID
	if command.Motion == nil {
		return ProcessStepResult{}, fmt.Errorf("skill: process motion step is required")
	}
	signals, finalize, err := host.applyMotionStepLocked(command.Motion, &state)
	if err != nil {
		return ProcessStepResult{}, err
	}
	host.processes[processID] = memoryProcess{state: state, active: state.Active}
	receipt := CommitReceipt{Revision: host.revision}
	if finalize {
		receipt = host.commitLocked("process_stepped", 0, processID)
	}
	return ProcessStepResult{Commit: receipt, State: state, Signals: signals}, nil
}

func (host *MemoryHost) StopProcess(command ProcessStopCommand, state ProcessHostState) (CommitReceipt, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return CommitReceipt{}, err
	}
	processID := command.Meta.ProcessID
	if processID == 0 {
		processID = state.ProcessID
	}
	process, exists := host.processes[processID]
	if !exists || !process.active {
		return CommitReceipt{Revision: host.revision}, nil
	}
	process.active = false
	process.state.Active = false
	host.processes[processID] = process
	return host.commitLocked("process_stopped", 0, processID), nil
}
