package entitysync

import (
	"encoding/binary"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// WireVersion is the subject-update encoding version. It changed when ARCH-10
// made frames per session: the outer room header (room frame number, room
// session sequence) is gone, a frame is a plain statesync frame whose
// Epoch/Tick are the session's own clock, and RoomID carries nothing but the
// stream constant below.
const WireVersion uint16 = 2

// wireStream is what goes into SnapshotMeta.RoomID. statesync requires it to
// be non-zero and equal between a delta and its base; a session has exactly
// one stream, so a constant does.
const wireStream uint64 = 1

const (
	subjectMagic       uint32 = 0x52535355 // "RSSU"
	subjectHeaderBytes        = 64
	subjectFlagFull    uint16 = 1 << 0
)

// EncodeSubjectUpdate serializes one subject update as the data of one
// statesync component. Namespace and profile key travel with it so a client
// can route on them without any out-of-band table.
func EncodeSubjectUpdate(update entity.SubjectSyncUpdate, maxBytes int) ([]byte, error) {
	profile := update.Profile.Normalize()
	namespace := []byte(update.Namespace)
	profileKey := []byte(profile.Key)
	if len(namespace) > int(^uint16(0)) || len(profileKey) > int(^uint16(0)) {
		return nil, frame.ErrComponentTooLarge
	}
	total := subjectHeaderBytes + len(namespace) + len(profileKey) + update.Payload.Len()
	if maxBytes <= 0 {
		maxBytes = frame.DefaultLimits().MaxComponentBytes
	}
	if total > maxBytes || update.SubjectID == 0 {
		return nil, frame.ErrComponentTooLarge
	}
	out := make([]byte, subjectHeaderBytes, total)
	binary.BigEndian.PutUint32(out[0:4], subjectMagic)
	binary.BigEndian.PutUint16(out[4:6], WireVersion)
	flags := uint16(0)
	if update.Full {
		flags |= subjectFlagFull
	}
	binary.BigEndian.PutUint16(out[6:8], flags)
	binary.BigEndian.PutUint64(out[8:16], uint64(update.SubjectID))
	binary.BigEndian.PutUint32(out[16:20], update.SubjectKind)
	binary.BigEndian.PutUint64(out[20:28], update.Version)
	binary.BigEndian.PutUint64(out[28:36], update.BaseVersion)
	binary.BigEndian.PutUint64(out[36:44], update.Mask)
	binary.BigEndian.PutUint32(out[44:48], update.Reason)
	binary.BigEndian.PutUint16(out[48:50], update.Payload.Codec())
	out[50] = profile.LOD
	binary.BigEndian.PutUint32(out[52:56], profile.SchemaVersion)
	binary.BigEndian.PutUint16(out[56:58], uint16(len(namespace)))
	binary.BigEndian.PutUint16(out[58:60], uint16(len(profileKey)))
	binary.BigEndian.PutUint32(out[60:64], uint32(update.Payload.Len()))
	out = append(out, namespace...)
	out = append(out, profileKey...)
	out = update.Payload.AppendTo(out)
	return out, nil
}

// DecodeSubjectUpdate is the inverse of EncodeSubjectUpdate; a client calls it
// on each component of a decoded frame.
func DecodeSubjectUpdate(data []byte, maxBytes int) (entity.SubjectSyncUpdate, error) {
	if maxBytes <= 0 {
		maxBytes = frame.DefaultLimits().MaxComponentBytes
	}
	if len(data) < subjectHeaderBytes || len(data) > maxBytes || binary.BigEndian.Uint32(data[0:4]) != subjectMagic || binary.BigEndian.Uint16(data[4:6]) != WireVersion {
		return entity.SubjectSyncUpdate{}, ErrWireFrame
	}
	flags := binary.BigEndian.Uint16(data[6:8])
	if flags & ^subjectFlagFull != 0 || data[51] != 0 {
		return entity.SubjectSyncUpdate{}, ErrWireFrame
	}
	namespaceLength := int(binary.BigEndian.Uint16(data[56:58]))
	profileLength := int(binary.BigEndian.Uint16(data[58:60]))
	payloadLength := int(binary.BigEndian.Uint32(data[60:64]))
	if subjectHeaderBytes+namespaceLength+profileLength+payloadLength != len(data) {
		return entity.SubjectSyncUpdate{}, ErrWireFrame
	}
	offset := subjectHeaderBytes
	update := entity.SubjectSyncUpdate{
		SubjectID: int64(binary.BigEndian.Uint64(data[8:16])), SubjectKind: binary.BigEndian.Uint32(data[16:20]),
		Version: binary.BigEndian.Uint64(data[20:28]), BaseVersion: binary.BigEndian.Uint64(data[28:36]),
		Mask: binary.BigEndian.Uint64(data[36:44]), Reason: binary.BigEndian.Uint32(data[44:48]), Full: flags&subjectFlagFull != 0,
		Namespace: string(data[offset : offset+namespaceLength]),
	}
	offset += namespaceLength
	update.Profile = entity.SyncProfile{Key: string(data[offset : offset+profileLength]), LOD: data[50], SchemaVersion: binary.BigEndian.Uint32(data[52:56])}.Normalize()
	offset += profileLength
	update.Payload = entity.CopyFrozenSyncPayload(binary.BigEndian.Uint16(data[48:50]), data[offset:offset+payloadLength])
	if update.SubjectID == 0 {
		return entity.SubjectSyncUpdate{}, ErrWireFrame
	}
	return update, nil
}

// DecodeFrame decodes one frame as pushed to a session: a statesync frame
// whose components are subject updates. Epoch/Tick are the session's clock.
func DecodeFrame(data []byte, limits frame.Limits) (frame.Frame, error) {
	return frame.Decode(data, limits)
}
