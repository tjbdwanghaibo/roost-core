package level

import (
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

type Phase string

const (
	Waiting   Phase = "waiting"
	Preparing Phase = "preparing"
	Running   Phase = "running"
	Finishing Phase = "finishing"
	Closing   Phase = "closing"
	Closed    Phase = "closed"
)

type StageState struct {
	Generation uint64
	StartedAt  int64
	Exiting    bool
}
type FireState struct {
	Count  uint32
	LastAt int64
}
type Timer struct {
	Name, Stage string
	Generation  uint64
	Due         int64
}
type SourceCursor struct {
	Sequence uint64
	Digest   string
}
type Effect struct {
	ID, Instance    string
	Epoch, Sequence uint64
	Stage           string
	Generation      uint64
	Name            string
	Args            map[string]Value
	LevelLifetime   bool
	Status          string // pending/done/cancelled；回执留存到实例关闭，不淘汰以复用 ID。
	Success         bool
}
type Event struct {
	Instance    string
	Epoch       uint64
	Source      string
	Sequence    uint64
	Kind, Stage string
	Generation  uint64
	Fields      map[string]Value
}
type Receipt struct {
	Instance string
	Epoch    uint64
	ID       string
	Success  bool
}
type State struct {
	Instance, DefinitionHash string
	Epoch, Revision          uint64
	Phase                    Phase
	Now, Deadline            int64
	Result                   string
	Active                   map[string]StageState
	Generations              map[string]uint64
	Variables                map[string]Value
	Fires                    map[string]FireState
	Timers                   map[string]Timer
	Sources                  map[string]SourceCursor
	Effects                  map[string]Effect
	NextEffect               uint64
}

func newState(instance string, epoch uint64, hash string, now int64) State {
	return State{Instance: instance, Epoch: epoch, DefinitionHash: hash, Phase: Waiting, Now: now, Active: map[string]StageState{}, Generations: map[string]uint64{}, Variables: map[string]Value{}, Fires: map[string]FireState{}, Timers: map[string]Timer{}, Sources: map[string]SourceCursor{}, Effects: map[string]Effect{}}
}
func cloneState(s State) State {
	s.Active = maps.Clone(s.Active)
	s.Generations = maps.Clone(s.Generations)
	s.Variables = maps.Clone(s.Variables)
	s.Fires = maps.Clone(s.Fires)
	s.Timers = maps.Clone(s.Timers)
	s.Sources = maps.Clone(s.Sources)
	s.Effects = maps.Clone(s.Effects)
	for id, e := range s.Effects {
		e.Args = maps.Clone(e.Args)
		s.Effects[id] = e
	}
	return s
}
func (s State) validate() error {
	if s.Instance == "" {
		if s.Now != 0 || s.Deadline != 0 || s.Result != "" || s.DefinitionHash != "" || s.NextEffect != 0 || s.Epoch != 0 || s.Revision != 0 || s.Phase != "" || len(s.Active)+len(s.Generations)+len(s.Variables)+len(s.Fires)+len(s.Timers)+len(s.Sources)+len(s.Effects) != 0 {
			return ErrState
		}
		return nil
	}
	if !identifier(s.Instance) || s.Epoch == 0 || len(s.DefinitionHash) != 64 || s.Now < 0 || s.Deadline < 0 || len(s.Active) > 32 || len(s.Generations) > 32 || len(s.Variables) > 8192 || len(s.Fires) > 2048 || len(s.Timers) > 128 || len(s.Sources) > 64 || len(s.Effects) > 4096 {
		return ErrState
	}
	if _, err := hex.DecodeString(s.DefinitionHash); err != nil {
		return ErrState
	}
	if s.Active == nil || s.Generations == nil || s.Variables == nil || s.Fires == nil || s.Timers == nil || s.Sources == nil || s.Effects == nil {
		return ErrState
	}
	for id, generation := range s.Generations {
		if !identifier(id) || generation == 0 {
			return ErrState
		}
	}
	switch s.Phase {
	case Waiting, Preparing, Running, Finishing, Closing, Closed:
	default:
		return ErrState
	}
	for name, stage := range s.Active {
		if !identifier(name) || stage.Generation == 0 || stage.Generation != s.Generations[name] || stage.Exiting || stage.StartedAt < 0 || stage.StartedAt > s.Now {
			return ErrState
		}
	}
	for _, v := range s.Variables {
		if !v.valid() {
			return ErrState
		}
	}
	for key, t := range s.Timers {
		stage, ok := s.Active[t.Stage]
		if !ok || stage.Generation != t.Generation || t.Due < 0 || key != t.Stage+"/"+t.Name || !identifier(t.Name) {
			return ErrState
		}
	}
	for source, cursor := range s.Sources {
		if !identifier(source) || cursor.Sequence == 0 || len(cursor.Digest) != 64 {
			return ErrState
		}
	}
	for id, e := range s.Effects {
		if id != effectID(s.Instance, s.Epoch, e.Sequence) || e.ID != id || e.Instance != s.Instance || e.Epoch != s.Epoch || e.Sequence == 0 || e.Sequence > s.NextEffect || e.Generation == 0 || !identifier(e.Stage) || !identifier(e.Name) || len(e.Args) > 32 {
			return ErrState
		}
		if e.Status != "pending" && e.Status != "done" && e.Status != "cancelled" {
			return ErrState
		}
		for _, v := range e.Args {
			if !v.valid() {
				return ErrState
			}
		}
		if e.Status == "pending" && !e.LevelLifetime {
			stage, ok := s.Active[e.Stage]
			if !ok || stage.Generation != e.Generation {
				return ErrState
			}
		}
	}
	if s.Phase != Running && len(s.Active) != 0 {
		return ErrState
	}
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > 2<<20 {
		return ErrBudget
	}
	return nil
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// validateProgram 在恢复后将存档键、类型和预算重新绑定到固定版本定义。
func (s State) validateProgram(p *Program) error {
	if s.Instance == "" {
		return nil
	}
	if s.DefinitionHash != p.hash {
		return ErrDefinition
	}
	limits := p.definition.Limits
	if len(s.Timers) > limits.Timers || len(s.Effects) > limits.Effects || len(s.Sources) > limits.Sources {
		return ErrBudget
	}
	for id := range s.Generations {
		if _, ok := p.stages[id]; !ok {
			return ErrState
		}
	}
	for key, v := range s.Variables {
		parts := strings.Split(key, "/")
		var scope Scope
		var name string
		switch {
		case len(parts) == 2 && parts[0] == "global":
			scope = Global
			name = parts[1]
		case len(parts) == 3 && parts[0] == "stage":
			scope = Stage
			name = parts[2]
			if _, ok := s.Active[parts[1]]; !ok {
				return ErrState
			}
		case len(parts) == 4 && parts[0] == "trigger":
			scope = Trigger
			name = parts[3]
			if _, ok := s.Active[parts[1]]; !ok || !p.hasTrigger(parts[1], parts[2]) {
				return ErrState
			}
		default:
			return ErrState
		}
		decl, ok := p.variables[variableKey(scope, name)]
		if !ok || decl.Default.Kind != v.Kind {
			return ErrState
		}
	}
	for key, f := range s.Fires {
		parts := strings.Split(key, "/")
		if len(parts) != 2 || !p.hasTrigger(parts[0], parts[1]) || f.Count == 0 || f.LastAt < 0 || f.LastAt > s.Now {
			return ErrState
		}
		if _, ok := s.Active[parts[0]]; !ok {
			return ErrState
		}
	}
	for _, e := range s.Effects {
		spec, ok := p.registry.Actions[e.Name]
		if !ok || !matches(spec.Args, e.Args) || s.Generations[e.Stage] < e.Generation {
			return ErrState
		}
		if (s.Phase == Closing || s.Phase == Closed) && e.Status == "pending" {
			return ErrState
		}
	}
	return nil
}
func (p *Program) hasTrigger(stage, id string) bool {
	for _, t := range p.stages[stage].Triggers {
		if t.ID == id {
			return true
		}
	}
	return false
}
