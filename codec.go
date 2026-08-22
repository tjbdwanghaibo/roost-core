package nestwal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/tjbdwanghaibo/cube-core/entity"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

const (
	codecVersion   = uint16(5)
	maxEntryCount  = 1 << 20
	maxStringBytes = 1 << 20
)

func encodeRecord(record corenest.CommitRecord) ([]byte, error) {
	if record.ID.IsZero() {
		return nil, errors.New("nestwal: zero transaction id")
	}
	if record.Empty() {
		return nil, errors.New("nestwal: empty commit record")
	}
	if record.Durability > corenest.DurabilityStrict {
		return nil, errors.New("nestwal: invalid durability policy")
	}
	if len(record.Mutations) > maxEntryCount || len(record.Effects) > maxEntryCount {
		return nil, errors.New("nestwal: too many entries in commit record")
	}
	for i := range record.Mutations {
		mutation := &record.Mutations[i]
		if mutation.EntityID == 0 || mutation.Resource == "" || (len(mutation.Data) == 0 && mutation.Remote == nil) {
			return nil, fmt.Errorf("nestwal: invalid mutation %d", i)
		}
	}
	for i := range record.Effects {
		effect := &record.Effects[i]
		if effect.ID == "" || effect.Topic == "" {
			return nil, fmt.Errorf("nestwal: invalid effect %d", i)
		}
	}
	b := bytes.NewBuffer(make([]byte, 0, recordSizeHint(record)))
	_ = binary.Write(b, binary.BigEndian, codecVersion)
	_, _ = b.Write(record.ID[:])
	_ = binary.Write(b, binary.BigEndian, record.CreatedAt)
	_ = b.WriteByte(byte(record.Durability))
	if err := writeString(b, record.Handler); err != nil {
		return nil, err
	}
	if err := writeString(b, record.RequestID); err != nil {
		return nil, err
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Mutations)))
	for i := range record.Mutations {
		m := &record.Mutations[i]
		_ = binary.Write(b, binary.BigEndian, m.EntityID)
		_ = binary.Write(b, binary.BigEndian, m.Version)
		_ = binary.Write(b, binary.BigEndian, m.Mask)
		_ = binary.Write(b, binary.BigEndian, m.Schema)
		if err := writeString(b, m.Database); err != nil {
			return nil, err
		}
		_ = b.WriteByte(m.DatabaseScope)
		if err := writeString(b, m.Resource); err != nil {
			return nil, err
		}
		if err := writeString(b, m.Codec); err != nil {
			return nil, err
		}
		if err := writeBytes(b, m.Data); err != nil {
			return nil, err
		}
		if err := writeRemoteCommit(b, m.Remote); err != nil {
			return nil, err
		}
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Effects)))
	for i := range record.Effects {
		e := &record.Effects[i]
		if err := writeString(b, e.ID); err != nil {
			return nil, err
		}
		if err := writeString(b, e.Topic); err != nil {
			return nil, err
		}
		if err := writeString(b, e.Key); err != nil {
			return nil, err
		}
		if err := writeBytes(b, e.Payload); err != nil {
			return nil, err
		}
		if len(e.Headers) > maxEntryCount {
			return nil, errors.New("nestwal: too many effect headers")
		}
		keys := make([]string, 0, len(e.Headers))
		for key := range e.Headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_ = binary.Write(b, binary.BigEndian, uint32(len(keys)))
		for _, key := range keys {
			if err := writeString(b, key); err != nil {
				return nil, err
			}
			if err := writeString(b, e.Headers[key]); err != nil {
				return nil, err
			}
		}
	}
	return b.Bytes(), nil
}

func decodeRecord(raw []byte) (corenest.CommitRecord, error) {
	r := bytes.NewReader(raw)
	var version uint16
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return corenest.CommitRecord{}, err
	}
	if version != codecVersion {
		return corenest.CommitRecord{}, fmt.Errorf("nestwal: unsupported record codec %d", version)
	}
	var record corenest.CommitRecord
	if _, err := io.ReadFull(r, record.ID[:]); err != nil {
		return record, err
	}
	if err := binary.Read(r, binary.BigEndian, &record.CreatedAt); err != nil {
		return record, err
	}
	durability, err := r.ReadByte()
	if err != nil {
		return record, err
	}
	record.Durability = corenest.DurabilityPolicy(durability)
	if record.Handler, err = readString(r); err != nil {
		return record, err
	}
	if record.RequestID, err = readString(r); err != nil {
		return record, err
	}
	mutationCount, err := readCount(r)
	if err != nil {
		return record, err
	}
	record.Mutations = make([]corenest.EntityMutation, mutationCount)
	for i := range record.Mutations {
		m := &record.Mutations[i]
		if err := binary.Read(r, binary.BigEndian, &m.EntityID); err != nil {
			return record, err
		}
		if err := binary.Read(r, binary.BigEndian, &m.Version); err != nil {
			return record, err
		}
		if err := binary.Read(r, binary.BigEndian, &m.Mask); err != nil {
			return record, err
		}
		if err := binary.Read(r, binary.BigEndian, &m.Schema); err != nil {
			return record, err
		}
		if m.Database, err = readString(r); err != nil {
			return record, err
		}
		if m.DatabaseScope, err = r.ReadByte(); err != nil {
			return record, err
		}
		if m.Resource, err = readString(r); err != nil {
			return record, err
		}
		if m.Codec, err = readString(r); err != nil {
			return record, err
		}
		if m.Data, err = readBytes(r); err != nil {
			return record, err
		}
		if m.Remote, err = readRemoteCommit(r); err != nil {
			return record, err
		}
	}
	effectCount, err := readCount(r)
	if err != nil {
		return record, err
	}
	record.Effects = make([]corenest.Effect, effectCount)
	for i := range record.Effects {
		e := &record.Effects[i]
		if e.ID, err = readString(r); err != nil {
			return record, err
		}
		if e.Topic, err = readString(r); err != nil {
			return record, err
		}
		if e.Key, err = readString(r); err != nil {
			return record, err
		}
		if e.Payload, err = readBytes(r); err != nil {
			return record, err
		}
		headerCount, readErr := readCount(r)
		if readErr != nil {
			return record, readErr
		}
		if headerCount > 0 {
			e.Headers = make(map[string]string, headerCount)
		}
		for range headerCount {
			key, readErr := readString(r)
			if readErr != nil {
				return record, readErr
			}
			value, readErr := readString(r)
			if readErr != nil {
				return record, readErr
			}
			e.Headers[key] = value
		}
	}
	if r.Len() != 0 {
		return record, fmt.Errorf("nestwal: %d trailing record bytes", r.Len())
	}
	return record, nil
}

func recordSizeHint(record corenest.CommitRecord) int {
	n := 64 + len(record.Handler) + len(record.RequestID)
	for i := range record.Mutations {
		m := &record.Mutations[i]
		n += 52 + len(m.Database) + len(m.Resource) + len(m.Codec) + len(m.Data)
	}
	for i := range record.Effects {
		e := &record.Effects[i]
		n += 32 + len(e.ID) + len(e.Topic) + len(e.Key) + len(e.Payload)
		for key, value := range e.Headers {
			n += 8 + len(key) + len(value)
		}
	}
	return n
}

func writeString(w *bytes.Buffer, value string) error {
	if len(value) > maxStringBytes {
		return errors.New("nestwal: string exceeds limit")
	}
	return writeBytes(w, []byte(value))
}

func writeBytes(w *bytes.Buffer, value []byte) error {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return errors.New("nestwal: byte field exceeds uint32")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(value)))
	_, _ = w.Write(value)
	return nil
}

func readString(r *bytes.Reader) (string, error) {
	raw, err := readBytes(r)
	if err != nil {
		return "", err
	}
	if len(raw) > maxStringBytes {
		return "", errors.New("nestwal: string exceeds limit")
	}
	return string(raw), nil
}

func readBytes(r *bytes.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	if uint64(size) > uint64(r.Len()) {
		return nil, io.ErrUnexpectedEOF
	}
	raw := make([]byte, int(size))
	_, err := io.ReadFull(r, raw)
	return raw, err
}

func readCount(r *bytes.Reader) (int, error) {
	var count uint32
	if err := binary.Read(r, binary.BigEndian, &count); err != nil {
		return 0, err
	}
	if count > maxEntryCount {
		return 0, errors.New("nestwal: entry count exceeds limit")
	}
	return int(count), nil
}

func writeRemoteCommit(w *bytes.Buffer, commit *entity.RemoteCommit) error {
	if commit == nil {
		return w.WriteByte(0)
	}
	if err := commit.Validate(); err != nil {
		return err
	}
	_ = w.WriteByte(1)
	_, _ = w.Write(commit.TransactionID[:])
	_ = binary.Write(w, binary.BigEndian, commit.EntityID)
	_ = binary.Write(w, binary.BigEndian, uint16(commit.Kind))
	if commit.Delete {
		_ = w.WriteByte(1)
	} else {
		_ = w.WriteByte(0)
	}
	_ = binary.Write(w, binary.BigEndian, commit.BaseVersion)
	_ = binary.Write(w, binary.BigEndian, commit.NextVersion)
	_ = binary.Write(w, binary.BigEndian, commit.MarkerEpoch)
	_ = binary.Write(w, binary.BigEndian, commit.LockFence)
	_ = binary.Write(w, binary.BigEndian, commit.RouteEpoch)
	_ = binary.Write(w, binary.BigEndian, commit.Schema)
	_ = binary.Write(w, binary.BigEndian, commit.Codec)
	_ = binary.Write(w, binary.BigEndian, commit.Checksum)
	if len(commit.Mutations) > maxEntryCount {
		return errors.New("nestwal: remote mutation count exceeds limit")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(commit.Mutations)))
	for i := range commit.Mutations {
		mutation := &commit.Mutations[i]
		if err := mutation.Validate(); err != nil {
			return err
		}
		if err := writeString(w, mutation.Database); err != nil {
			return err
		}
		_ = w.WriteByte(mutation.DatabaseScope)
		if err := writeString(w, mutation.Collection); err != nil {
			return err
		}
		_ = binary.Write(w, binary.BigEndian, mutation.ID)
		_ = binary.Write(w, binary.BigEndian, mutation.Version)
		_ = binary.Write(w, binary.BigEndian, mutation.Mask)
		if err := writeBytes(w, mutation.Data); err != nil {
			return err
		}
	}
	if len(commit.Deletes) > maxEntryCount {
		return errors.New("nestwal: remote delete count exceeds limit")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(commit.Deletes)))
	for i := range commit.Deletes {
		item := &commit.Deletes[i]
		if err := item.Validate(); err != nil {
			return err
		}
		if err := writeString(w, item.Database); err != nil {
			return err
		}
		_ = w.WriteByte(item.DatabaseScope)
		if err := writeString(w, item.Collection); err != nil {
			return err
		}
		_ = binary.Write(w, binary.BigEndian, item.ID)
	}
	if len(commit.Snapshots) > maxEntryCount {
		return errors.New("nestwal: remote snapshot count exceeds limit")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(commit.Snapshots)))
	for i := range commit.Snapshots {
		s := &commit.Snapshots[i]
		_ = binary.Write(w, binary.BigEndian, s.Key.Tenant)
		_ = binary.Write(w, binary.BigEndian, s.Key.EntityID)
		_ = binary.Write(w, binary.BigEndian, uint16(s.Key.Kind))
		_ = binary.Write(w, binary.BigEndian, s.Key.Scope)
		_ = binary.Write(w, binary.BigEndian, s.Key.Policy)
		_ = binary.Write(w, binary.BigEndian, s.BaseVersion)
		_ = binary.Write(w, binary.BigEndian, s.StateVersion)
		_ = binary.Write(w, binary.BigEndian, s.MarkerEpoch)
		_ = binary.Write(w, binary.BigEndian, s.RouteEpoch)
		_ = binary.Write(w, binary.BigEndian, s.Schema)
		_ = binary.Write(w, binary.BigEndian, s.Codec)
		if s.Full {
			_ = w.WriteByte(1)
		} else {
			_ = w.WriteByte(0)
		}
		_ = binary.Write(w, binary.BigEndian, s.Checksum)
		if err := writeBytes(w, s.Data); err != nil {
			return err
		}
	}
	if len(commit.Invalidations) > maxEntryCount {
		return errors.New("nestwal: remote invalidation count exceeds limit")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(commit.Invalidations)))
	for _, key := range commit.Invalidations {
		_ = binary.Write(w, binary.BigEndian, key.Tenant)
		_ = binary.Write(w, binary.BigEndian, key.EntityID)
		_ = binary.Write(w, binary.BigEndian, uint16(key.Kind))
		_ = binary.Write(w, binary.BigEndian, key.Scope)
		_ = binary.Write(w, binary.BigEndian, key.Policy)
	}
	return nil
}

func readRemoteCommit(r *bytes.Reader) (*entity.RemoteCommit, error) {
	present, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	if present != 1 {
		return nil, errors.New("nestwal: invalid remote commit marker")
	}
	commit := &entity.RemoteCommit{}
	if _, err := io.ReadFull(r, commit.TransactionID[:]); err != nil {
		return nil, err
	}
	var kind uint16
	var deleted byte
	if err := binary.Read(r, binary.BigEndian, &commit.EntityID); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.BigEndian, &kind); err != nil {
		return nil, err
	}
	commit.Kind = entity.EntityKind(kind)
	if deleted, err = r.ReadByte(); err != nil {
		return nil, err
	}
	commit.Delete = deleted == 1
	fields := []any{&commit.BaseVersion, &commit.NextVersion, &commit.MarkerEpoch, &commit.LockFence, &commit.RouteEpoch, &commit.Schema, &commit.Codec, &commit.Checksum}
	for _, field := range fields {
		if err := binary.Read(r, binary.BigEndian, field); err != nil {
			return nil, err
		}
	}
	mutationCount, err := readCount(r)
	if err != nil {
		return nil, err
	}
	commit.Mutations = make([]entity.RemoteDataMutation, mutationCount)
	for i := range commit.Mutations {
		mutation := &commit.Mutations[i]
		if mutation.Database, err = readString(r); err != nil {
			return nil, err
		}
		if mutation.DatabaseScope, err = r.ReadByte(); err != nil {
			return nil, err
		}
		if mutation.Collection, err = readString(r); err != nil {
			return nil, err
		}
		for _, field := range []any{&mutation.ID, &mutation.Version, &mutation.Mask} {
			if err := binary.Read(r, binary.BigEndian, field); err != nil {
				return nil, err
			}
		}
		if mutation.Data, err = readBytes(r); err != nil {
			return nil, err
		}
	}
	deleteCount, err := readCount(r)
	if err != nil {
		return nil, err
	}
	commit.Deletes = make([]entity.RemoteDataDelete, deleteCount)
	for i := range commit.Deletes {
		item := &commit.Deletes[i]
		if item.Database, err = readString(r); err != nil {
			return nil, err
		}
		if item.DatabaseScope, err = r.ReadByte(); err != nil {
			return nil, err
		}
		if item.Collection, err = readString(r); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.BigEndian, &item.ID); err != nil {
			return nil, err
		}
	}
	count, err := readCount(r)
	if err != nil {
		return nil, err
	}
	commit.Snapshots = make([]entity.RemoteSnapshotRecord, count)
	for i := range commit.Snapshots {
		s := &commit.Snapshots[i]
		var snapshotKind uint16
		fields := []any{&s.Key.Tenant, &s.Key.EntityID, &snapshotKind, &s.Key.Scope, &s.Key.Policy, &s.BaseVersion, &s.StateVersion, &s.MarkerEpoch, &s.RouteEpoch, &s.Schema, &s.Codec}
		for _, field := range fields {
			if err := binary.Read(r, binary.BigEndian, field); err != nil {
				return nil, err
			}
		}
		s.Key.Kind = entity.EntityKind(snapshotKind)
		full, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		s.Full = full == 1
		if err := binary.Read(r, binary.BigEndian, &s.Checksum); err != nil {
			return nil, err
		}
		if s.Data, err = readBytes(r); err != nil {
			return nil, err
		}
	}
	invalidationCount, err := readCount(r)
	if err != nil {
		return nil, err
	}
	commit.Invalidations = make([]entity.RemoteSnapshotKey, invalidationCount)
	for i := range commit.Invalidations {
		key := &commit.Invalidations[i]
		var kind uint16
		for _, field := range []any{&key.Tenant, &key.EntityID, &kind, &key.Scope, &key.Policy} {
			if err := binary.Read(r, binary.BigEndian, field); err != nil {
				return nil, err
			}
		}
		key.Kind = entity.EntityKind(kind)
	}
	if err := commit.Validate(); err != nil {
		return nil, err
	}
	return commit, nil
}
