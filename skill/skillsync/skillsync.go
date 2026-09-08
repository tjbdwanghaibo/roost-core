// Package skillsync projects skill runtime data onto roost-core's ordered,
// transport-neutral syncstream envelopes.
package skillsync

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

const (
	TopicManifest     = "roost.skill.manifest"
	TopicState        = "roost.skill.state"
	TopicPresentation = "roost.skill.presentation"
)

var (
	ErrSchemaVersionRequired = errors.New("skillsync: schema version is required")
	ErrRecordInvalid         = errors.New("skillsync: record is invalid")
)

type RecordKind string

const (
	RecordManifest          RecordKind = "manifest"
	RecordStateFull         RecordKind = "state_full"
	RecordStateDelta        RecordKind = "state_delta"
	RecordPresentation      RecordKind = "presentation"
	RecordPresentationReset RecordKind = "presentation_reset"
)

type Header struct {
	SchemaVersion      uint32              `json:"schema_version"`
	Kind               RecordKind          `json:"kind"`
	Tick               skill.Tick          `json:"tick,omitempty"`
	WorldRevision      skill.WorldRevision `json:"world_revision,omitempty"`
	GameplayDigest     string              `json:"gameplay_digest,omitempty"`
	PresentationDigest string              `json:"presentation_digest,omitempty"`
}

type ManifestRecord struct {
	Header
	Plan skill.PresentationPlan `json:"plan"`
}

type StateSnapshot = skill.RuntimeStateSnapshot

type StateRecord struct {
	Header
	Snapshot *skill.RuntimeStateSnapshot `json:"snapshot,omitempty"`
	Delta    *skill.StateMutation        `json:"delta,omitempty"`
}

type PresentationReset struct {
	Recovery skill.PresentationRecoverySnapshot `json:"recovery"`
}

type PresentationRecord struct {
	Header
	Event *skill.PresentationEvent `json:"event,omitempty"`
	Reset *PresentationReset       `json:"reset,omitempty"`
}

type Projector struct {
	SchemaVersion uint32
}

func NewProjector(schemaVersion uint32) (Projector, error) {
	if schemaVersion == 0 {
		return Projector{}, ErrSchemaVersionRequired
	}
	return Projector{SchemaVersion: schemaVersion}, nil
}

func (projector Projector) ManifestPacket(observer syncstream.Observer, key int64, plan skill.PresentationPlan) (syncstream.Packet, error) {
	record := ManifestRecord{Header: Header{
		SchemaVersion: projector.SchemaVersion, Kind: RecordManifest,
		GameplayDigest: plan.Identity.GameplayDigest, PresentationDigest: plan.Identity.PresentationDigest,
	}, Plan: plan}
	return projector.packet(observer, key, TopicManifest, true, true, record)
}

func (projector Projector) StateSnapshotPacket(observer syncstream.Observer, key int64, snapshot skill.RuntimeStateSnapshot) (syncstream.Packet, error) {
	record := StateRecord{Header: Header{
		SchemaVersion: projector.SchemaVersion, Kind: RecordStateFull,
		Tick: snapshot.Tick, WorldRevision: snapshot.WorldRevision,
	}, Snapshot: &snapshot}
	return projector.packet(observer, key, TopicState, true, true, record)
}

func (projector Projector) StateDeltaPacket(observer syncstream.Observer, key int64, delta skill.StateMutation) (syncstream.Packet, error) {
	if delta.Sequence == 0 || delta.Kind == "" {
		return syncstream.Packet{}, fmt.Errorf("%w: state delta requires sequence and event kind", ErrRecordInvalid)
	}
	record := StateRecord{Header: Header{
		SchemaVersion: projector.SchemaVersion, Kind: RecordStateDelta,
		Tick: delta.Tick, WorldRevision: delta.WorldRevision,
	}, Delta: &delta}
	return projector.packet(observer, key, TopicState, false, true, record)
}

func (projector Projector) PresentationPacket(observer syncstream.Observer, key int64, event skill.PresentationEvent) (syncstream.Packet, error) {
	if event.Sequence == 0 || event.Kind == "" {
		return syncstream.Packet{}, fmt.Errorf("%w: presentation event requires sequence and kind", ErrRecordInvalid)
	}
	record := PresentationRecord{Header: Header{
		SchemaVersion: projector.SchemaVersion, Kind: RecordPresentation,
		Tick: event.Tick, WorldRevision: event.WorldRevision,
		GameplayDigest: event.GameplayDigest, PresentationDigest: event.PresentationDigest,
	}, Event: &event}
	return projector.packet(observer, key, TopicPresentation, false, false, record)
}

func (projector Projector) PresentationResetPacket(observer syncstream.Observer, key int64, snapshot skill.PresentationRecoverySnapshot) (syncstream.Packet, error) {
	reset := PresentationReset{Recovery: snapshot}
	record := PresentationRecord{Header: Header{SchemaVersion: projector.SchemaVersion, Kind: RecordPresentationReset, Tick: snapshot.Tick, WorldRevision: snapshot.WorldRevision}, Reset: &reset}
	return projector.packet(observer, key, TopicPresentation, true, true, record)
}

func (projector Projector) packet(observer syncstream.Observer, key int64, topic string, full, critical bool, record any) (syncstream.Packet, error) {
	if projector.SchemaVersion == 0 {
		return syncstream.Packet{}, ErrSchemaVersionRequired
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return syncstream.Packet{}, err
	}
	return syncstream.Packet{
		Observer: observer, Stream: syncstream.Stream{Topic: topic, Key: key},
		SchemaVersion: projector.SchemaVersion, Full: full, Critical: critical, Payload: payload,
	}, nil
}
