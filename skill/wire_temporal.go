package skill

import "encoding/json"

type CaptureSnapshotEffectDefinition struct {
	Target  Value
	Profile string
}

func (CaptureSnapshotEffectDefinition) effectDefinition() {}

type RestoreSnapshotEffectDefinition struct {
	Target, Snapshot Value
	OnBlocked        string
}

func (RestoreSnapshotEffectDefinition) effectDefinition() {}

func decodeCaptureSnapshotEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type    string          `json:"type"`
		Target  json.RawMessage `json:"target"`
		Profile string          `json:"profile"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return CaptureSnapshotEffectDefinition{Target: target, Profile: raw.Profile}, nil
}

func decodeRestoreSnapshotEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type      string          `json:"type"`
		Target    json.RawMessage `json:"target"`
		Snapshot  json.RawMessage `json:"snapshot"`
		OnBlocked string          `json:"on_blocked"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Snapshot)
	if err != nil {
		return nil, err
	}
	return RestoreSnapshotEffectDefinition{Target: values[0], Snapshot: values[1], OnBlocked: raw.OnBlocked}, nil
}
