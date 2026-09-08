package skill

import (
	"encoding/json"
	"fmt"
)

// ProcessDefinition owns its motion plan. Motion is deliberately not a flow or
// effect variant, so callers cannot reorder its stages.
type ProcessDefinition struct {
	Kind            string
	DurationTicks   Tick
	IntervalTicks   Tick
	EmitLeaveOnStop bool
	Visual          *VisualRef
	Area            *SelectDefinition
	Motion          *MotionDefinition
	NumericTracks   []NumericTrackDefinition
}

type MotionDefinition struct {
	Frame      FrameDefinition
	Steering   SteeringDefinition
	Trajectory TrajectoryDefinition
	Offsets    []OffsetDefinition
	Collision  *MotionCollisionDefinition
	Carry      *CarryDefinition
	Completion CompletionDefinition
}

type FrameDefinition interface{ frameDefinition() }
type WorldFrameDefinition struct{}

func (WorldFrameDefinition) frameDefinition() {}

type FollowFrameDefinition struct{ Target Value }

func (FollowFrameDefinition) frameDefinition() {}

type SteeringDefinition interface{ steeringDefinition() }
type TrackingSteeringDefinition struct {
	Target        Value
	DurationTicks Tick
}

func (TrackingSteeringDefinition) steeringDefinition() {}

type TrajectoryDefinition interface{ trajectoryDefinition() }
type StationaryTrajectoryDefinition struct{}

func (StationaryTrajectoryDefinition) trajectoryDefinition() {}

type LinearTrajectoryDefinition struct{ Speed Value }

func (LinearTrajectoryDefinition) trajectoryDefinition() {}

type PathTrajectoryDefinition struct{ Points, Speed Value }

func (PathTrajectoryDefinition) trajectoryDefinition() {}

type OrbitTrajectoryDefinition struct{ Anchor, Radius, AngularSpeed Value }

func (OrbitTrajectoryDefinition) trajectoryDefinition() {}

type ParabolaTrajectoryDefinition struct {
	Destination, Height Value
	DurationTicks       Tick
}

func (ParabolaTrajectoryDefinition) trajectoryDefinition() {}

type OffsetDefinition interface{ offsetDefinition() }
type ZigzagOffsetDefinition struct {
	Amplitude   Value
	PeriodTicks Tick
}

func (ZigzagOffsetDefinition) offsetDefinition() {}

type CircularOffsetDefinition struct{ Radius, AngularSpeed Value }

func (CircularOffsetDefinition) offsetDefinition() {}

type MotionCollisionDefinition struct {
	Layers                  []string
	Response                string
	MaxReflects, MaxPierces int
}
type CarryDefinition struct{ Target Value }

type CompletionDefinition interface{ completionDefinition() }
type EndCompletionDefinition struct{}

func (EndCompletionDefinition) completionDefinition() {}

type PauseThenEndCompletionDefinition struct{ PauseTicks Tick }

func (PauseThenEndCompletionDefinition) completionDefinition() {}

type BoomerangCompletionDefinition struct{ MaxReturnTicks Tick }

func (BoomerangCompletionDefinition) completionDefinition() {}

func decodeProcess(data []byte) (*ProcessDefinition, error) {
	var raw struct {
		Kind            string          `json:"kind"`
		DurationTicks   Tick            `json:"duration_ticks"`
		IntervalTicks   Tick            `json:"interval_ticks"`
		EmitLeaveOnStop bool            `json:"emit_leave_on_stop"`
		Visual          *VisualRef      `json:"visual"`
		Area            json.RawMessage `json:"area"`
		Motion          json.RawMessage `json:"motion"`
		NumericTracks   []struct {
			Property  string          `json:"property"`
			Operation string          `json:"operation"`
			Value     json.RawMessage `json:"value"`
			OverTicks Tick            `json:"over_ticks"`
		} `json:"numeric_tracks"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	motion, err := decodeOptionalMotion(raw.Motion)
	if err != nil {
		return nil, fmt.Errorf("motion: %w", err)
	}
	var area *SelectDefinition
	if len(raw.Area) != 0 && string(raw.Area) != "null" {
		decoded, decodeErr := decodeSelect(raw.Area)
		if decodeErr != nil {
			return nil, fmt.Errorf("area: %w", decodeErr)
		}
		area = &decoded
	}
	tracks := make([]NumericTrackDefinition, len(raw.NumericTracks))
	for index, track := range raw.NumericTracks {
		value, decodeErr := decodeValue(track.Value)
		if decodeErr != nil {
			return nil, fmt.Errorf("numeric_tracks[%d].value: %w", index, decodeErr)
		}
		tracks[index] = NumericTrackDefinition{Property: track.Property, Operation: track.Operation, Value: value, OverTicks: track.OverTicks}
	}
	return &ProcessDefinition{Kind: raw.Kind, DurationTicks: raw.DurationTicks, IntervalTicks: raw.IntervalTicks, EmitLeaveOnStop: raw.EmitLeaveOnStop, Visual: raw.Visual, Area: area, Motion: motion, NumericTracks: tracks}, nil
}

func decodeOptionalMotion(data json.RawMessage) (*MotionDefinition, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var raw struct {
		Frame      json.RawMessage   `json:"frame"`
		Steering   json.RawMessage   `json:"steering"`
		Trajectory json.RawMessage   `json:"trajectory"`
		Offsets    []json.RawMessage `json:"offsets"`
		Collision  json.RawMessage   `json:"collision"`
		Carry      json.RawMessage   `json:"carry"`
		Completion json.RawMessage   `json:"completion"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	if len(raw.Frame) == 0 || len(raw.Trajectory) == 0 || len(raw.Completion) == 0 {
		return nil, fmt.Errorf("frame, trajectory, and completion are required")
	}
	frame, err := decodeFrame(raw.Frame)
	if err != nil {
		return nil, fmt.Errorf("frame: %w", err)
	}
	steering, err := decodeOptionalSteering(raw.Steering)
	if err != nil {
		return nil, fmt.Errorf("steering: %w", err)
	}
	trajectory, err := decodeTrajectory(raw.Trajectory)
	if err != nil {
		return nil, fmt.Errorf("trajectory: %w", err)
	}
	offsets, err := decodeOffsets(raw.Offsets)
	if err != nil {
		return nil, fmt.Errorf("offsets: %w", err)
	}
	collision, err := decodeOptionalCollision(raw.Collision)
	if err != nil {
		return nil, fmt.Errorf("collision: %w", err)
	}
	carry, err := decodeOptionalCarry(raw.Carry)
	if err != nil {
		return nil, fmt.Errorf("carry: %w", err)
	}
	completion, err := decodeCompletion(raw.Completion)
	if err != nil {
		return nil, fmt.Errorf("completion: %w", err)
	}
	return &MotionDefinition{Frame: frame, Steering: steering, Trajectory: trajectory, Offsets: offsets, Collision: collision, Carry: carry, Completion: completion}, nil
}

func decodeFrame(data json.RawMessage) (FrameDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "world":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return WorldFrameDefinition{}, nil
	case "follow":
		var raw struct {
			Type   string          `json:"type"`
			Target json.RawMessage `json:"target"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		target, err := decodeValue(raw.Target)
		if err != nil {
			return nil, err
		}
		return FollowFrameDefinition{Target: target}, nil
	default:
		return nil, fmt.Errorf("unsupported frame %q", header.Type)
	}
}

func decodeOptionalSteering(data json.RawMessage) (SteeringDefinition, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	if header.Type != "tracking" {
		return nil, fmt.Errorf("unsupported steering %q", header.Type)
	}
	var raw struct {
		Type          string          `json:"type"`
		Target        json.RawMessage `json:"target"`
		DurationTicks Tick            `json:"duration_ticks"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return TrackingSteeringDefinition{Target: target, DurationTicks: raw.DurationTicks}, nil
}

func decodeTrajectory(data json.RawMessage) (TrajectoryDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "stationary":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StationaryTrajectoryDefinition{}, nil
	case "linear":
		var raw struct {
			Type  string          `json:"type"`
			Speed json.RawMessage `json:"speed"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		speed, err := decodeValue(raw.Speed)
		if err != nil {
			return nil, err
		}
		return LinearTrajectoryDefinition{Speed: speed}, nil
	case "path":
		var raw struct {
			Type   string          `json:"type"`
			Points json.RawMessage `json:"points"`
			Speed  json.RawMessage `json:"speed"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Points, raw.Speed)
		if err != nil {
			return nil, err
		}
		return PathTrajectoryDefinition{Points: values[0], Speed: values[1]}, nil
	case "orbit":
		var raw struct {
			Type         string          `json:"type"`
			Anchor       json.RawMessage `json:"anchor"`
			Radius       json.RawMessage `json:"radius"`
			AngularSpeed json.RawMessage `json:"angular_speed"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Anchor, raw.Radius, raw.AngularSpeed)
		if err != nil {
			return nil, err
		}
		return OrbitTrajectoryDefinition{Anchor: values[0], Radius: values[1], AngularSpeed: values[2]}, nil
	case "parabola":
		var raw struct {
			Type          string          `json:"type"`
			Destination   json.RawMessage `json:"destination"`
			Height        json.RawMessage `json:"height"`
			DurationTicks Tick            `json:"duration_ticks"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Destination, raw.Height)
		if err != nil {
			return nil, err
		}
		return ParabolaTrajectoryDefinition{Destination: values[0], Height: values[1], DurationTicks: raw.DurationTicks}, nil
	default:
		return nil, fmt.Errorf("unsupported trajectory %q", header.Type)
	}
}

func decodeOffsets(data []json.RawMessage) ([]OffsetDefinition, error) {
	result := make([]OffsetDefinition, len(data))
	for i, item := range data {
		offset, err := decodeOffset(item)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		result[i] = offset
	}
	return result, nil
}
func decodeOffset(data json.RawMessage) (OffsetDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "zigzag":
		var raw struct {
			Type        string          `json:"type"`
			Amplitude   json.RawMessage `json:"amplitude"`
			PeriodTicks Tick            `json:"period_ticks"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		amplitude, err := decodeValue(raw.Amplitude)
		if err != nil {
			return nil, err
		}
		return ZigzagOffsetDefinition{Amplitude: amplitude, PeriodTicks: raw.PeriodTicks}, nil
	case "circular":
		var raw struct {
			Type         string          `json:"type"`
			Radius       json.RawMessage `json:"radius"`
			AngularSpeed json.RawMessage `json:"angular_speed"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Radius, raw.AngularSpeed)
		if err != nil {
			return nil, err
		}
		return CircularOffsetDefinition{Radius: values[0], AngularSpeed: values[1]}, nil
	default:
		return nil, fmt.Errorf("unsupported offset %q", header.Type)
	}
}
func decodeOptionalCollision(data json.RawMessage) (*MotionCollisionDefinition, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var raw struct {
		Layers      []string `json:"layers"`
		Response    string   `json:"response"`
		MaxReflects int      `json:"max_reflects"`
		MaxPierces  int      `json:"max_pierces"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	return &MotionCollisionDefinition{Layers: raw.Layers, Response: raw.Response, MaxReflects: raw.MaxReflects, MaxPierces: raw.MaxPierces}, nil
}
func decodeOptionalCarry(data json.RawMessage) (*CarryDefinition, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var raw struct {
		Target json.RawMessage `json:"target"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return &CarryDefinition{Target: target}, nil
}
func decodeCompletion(data json.RawMessage) (CompletionDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "end":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return EndCompletionDefinition{}, nil
	case "pause_then_end":
		var raw struct {
			Type       string `json:"type"`
			PauseTicks Tick   `json:"pause_ticks"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return PauseThenEndCompletionDefinition{PauseTicks: raw.PauseTicks}, nil
	case "boomerang":
		var raw struct {
			Type           string `json:"type"`
			MaxReturnTicks Tick   `json:"max_return_ticks"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return BoomerangCompletionDefinition{MaxReturnTicks: raw.MaxReturnTicks}, nil
	default:
		return nil, fmt.Errorf("unsupported completion %q", header.Type)
	}
}
