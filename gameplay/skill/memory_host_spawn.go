package skill

import "fmt"

func (host *MemoryHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return SpawnStepResult{}, err
	}
	spawnID := command.Meta.SpawnID
	if spawnID == 0 {
		spawnID = state.SpawnID
	}
	if spawnID == 0 {
		return SpawnStepResult{}, fmt.Errorf("skill: spawn id is required")
	}
	state.SpawnID = spawnID
	if command.Motion == nil {
		return SpawnStepResult{}, fmt.Errorf("skill: spawn motion step is required")
	}
	signals, finalize, err := host.applyMotionStepLocked(command.Motion, &state)
	if err != nil {
		return SpawnStepResult{}, err
	}
	host.spawns[spawnID] = memorySpawn{state: state, active: state.Active}
	receipt := CommitReceipt{Revision: host.revision}
	if finalize {
		receipt = host.commitLocked("spawn_stepped", 0, spawnID)
	}
	return SpawnStepResult{Commit: receipt, State: state, Signals: signals}, nil
}

func (host *MemoryHost) StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return CommitReceipt{}, err
	}
	spawnID := command.Meta.SpawnID
	if spawnID == 0 {
		spawnID = state.SpawnID
	}
	spawn, exists := host.spawns[spawnID]
	if !exists || !spawn.active {
		return CommitReceipt{Revision: host.revision}, nil
	}
	spawn.active = false
	spawn.state.Active = false
	host.spawns[spawnID] = spawn
	return host.commitLocked("spawn_stopped", 0, spawnID), nil
}
