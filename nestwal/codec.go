package nestwal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

const (
	// codecVersionV1 is the already-deployed Nest WAL wire version. The public
	// writer versions are rollout controls and intentionally do not renumber
	// existing bytes on disk.
	codecVersionV1 = uint16(5)
	codecVersionV2 = uint16(6)
	maxEntryCount  = 1 << 20
	maxStringBytes = 1 << 20
)

type WriterVersion uint16

const (
	WriterVersionV1 WriterVersion = 1
	WriterVersionV2 WriterVersion = 2
)

var (
	ErrUnsupportedRecordVersion = errors.New("nestwal: unsupported record version")
	ErrWriterVersionUnsupported = errors.New("nestwal: record requires writer v2")
)

func encodeRecord(record corenest.CommitRecord) ([]byte, error) {
	return encodeRecordVersion(record, WriterVersionV1)
}

func encodeRecordVersion(record corenest.CommitRecord, writerVersion WriterVersion) ([]byte, error) {
	canonical, err := canonicalizeRecord(record)
	if err != nil {
		return nil, err
	}
	switch writerVersion {
	case WriterVersionV1:
		return encodeRecordV1(canonical)
	case WriterVersionV2:
		return encodeRecordV2(canonical)
	default:
		return nil, fmt.Errorf("nestwal: invalid writer version %d", writerVersion)
	}
}

func canonicalizeRecord(record corenest.CommitRecord) (corenest.CommitRecord, error) {
	if record.Empty() {
		return record, errors.New("nestwal: empty commit record")
	}
	if record.Durability > corenest.DurabilityPipelined {
		return record, errors.New("nestwal: invalid durability policy")
	}
	if len(record.Mutations) > maxEntryCount || len(record.Effects) > maxEntryCount || len(record.Receipts) > maxEntryCount {
		return record, errors.New("nestwal: too many entries in commit record")
	}
	record = dataengine.CloneCommitRecord(record)
	for i := range record.Mutations {
		mutation, err := dataengine.CanonicalizeMutation(record.Mutations[i])
		if err != nil {
			return record, fmt.Errorf("nestwal: canonicalize mutation %d: %w", i, err)
		}
		record.Mutations[i] = mutation
	}
	if err := dataengine.ValidateCommitRecord(record); err != nil {
		return record, err
	}
	return record, nil
}

func encodeRecordHeader(b *bytes.Buffer, codec uint16, record corenest.CommitRecord) error {
	_ = binary.Write(b, binary.BigEndian, codec)
	_, _ = b.Write(record.ID[:])
	_ = binary.Write(b, binary.BigEndian, record.CreatedAt)
	_ = b.WriteByte(byte(record.Durability))
	if err := writeString(b, record.Handler); err != nil {
		return err
	}
	if err := writeString(b, record.RequestID); err != nil {
		return err
	}
	return nil
}

func encodeRecordV1(record corenest.CommitRecord) ([]byte, error) {
	if len(record.Receipts) != 0 {
		return nil, ErrWriterVersionUnsupported
	}
	for _, effect := range record.Effects {
		if effect.AvailableAt != 0 {
			return nil, ErrWriterVersionUnsupported
		}
	}
	for _, mutation := range record.Mutations {
		if mutation.Remote == nil && mutation.Kind != dataengine.MutationPut {
			return nil, ErrWriterVersionUnsupported
		}
	}
	b := bytes.NewBuffer(make([]byte, 0, recordSizeHint(record)))
	if err := encodeRecordHeader(b, codecVersionV1, record); err != nil {
		return nil, err
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Mutations)))
	for i := range record.Mutations {
		m := &record.Mutations[i]
		_ = binary.Write(b, binary.BigEndian, m.Key.ID)
		_ = binary.Write(b, binary.BigEndian, m.NextVersion)
		_ = binary.Write(b, binary.BigEndian, m.Mask)
		_ = binary.Write(b, binary.BigEndian, m.Schema)
		if err := writeString(b, m.Key.Database); err != nil {
			return nil, err
		}
		_ = b.WriteByte(byte(m.Key.Scope))
		if err := writeString(b, m.Key.Resource); err != nil {
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
	if err := writeEffects(b, record.Effects, false); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encodeRecordV2(record corenest.CommitRecord) ([]byte, error) {
	b := bytes.NewBuffer(make([]byte, 0, recordSizeHint(record)))
	if err := encodeRecordHeader(b, codecVersionV2, record); err != nil {
		return nil, err
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Mutations)))
	for i := range record.Mutations {
		m := &record.Mutations[i]
		_ = binary.Write(b, binary.BigEndian, m.Key.ID)
		_ = binary.Write(b, binary.BigEndian, m.ExpectedVersion)
		_ = binary.Write(b, binary.BigEndian, m.NextVersion)
		_ = binary.Write(b, binary.BigEndian, m.Mask)
		_ = binary.Write(b, binary.BigEndian, m.Schema)
		_ = b.WriteByte(byte(m.Kind))
		_ = b.WriteByte(byte(m.Key.Scope))
		if err := writeString(b, m.Key.Database); err != nil {
			return nil, err
		}
		if err := writeString(b, m.Key.Resource); err != nil {
			return nil, err
		}
		if err := writeString(b, m.Codec); err != nil {
			return nil, err
		}
		if err := writeBytes(b, m.Data); err != nil {
			return nil, err
		}
		if err := writeBytes(b, m.Patch.SetBSON); err != nil {
			return nil, err
		}
		if len(m.Patch.Unset) > maxEntryCount {
			return nil, errors.New("nestwal: too many unset paths")
		}
		_ = binary.Write(b, binary.BigEndian, uint32(len(m.Patch.Unset)))
		for _, path := range m.Patch.Unset {
			if err := writeString(b, path); err != nil {
				return nil, err
			}
		}
		if err := writeRemoteCommit(b, m.Remote); err != nil {
			return nil, err
		}
	}
	if err := writeEffects(b, record.Effects, true); err != nil {
		return nil, err
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Receipts)))
	for i := range record.Receipts {
		receipt := &record.Receipts[i]
		if err := writeString(b, receipt.Namespace); err != nil {
			return nil, err
		}
		if err := writeString(b, receipt.ID); err != nil {
			return nil, err
		}
		if err := writeBytes(b, receipt.Digest); err != nil {
			return nil, err
		}
		if err := writeBytes(b, receipt.Payload); err != nil {
			return nil, err
		}
		_ = binary.Write(b, binary.BigEndian, receipt.ExpiresAt)
	}
	return b.Bytes(), nil
}

func writeEffects(b *bytes.Buffer, effects []corenest.Effect, includeAvailableAt bool) error {
	_ = binary.Write(b, binary.BigEndian, uint32(len(effects)))
	for i := range effects {
		e := &effects[i]
		if err := writeString(b, e.ID); err != nil {
			return err
		}
		if err := writeString(b, e.Topic); err != nil {
			return err
		}
		if err := writeString(b, e.Key); err != nil {
			return err
		}
		if err := writeBytes(b, e.Payload); err != nil {
			return err
		}
		if includeAvailableAt {
			_ = binary.Write(b, binary.BigEndian, e.AvailableAt)
		}
		if len(e.Headers) > maxEntryCount {
			return errors.New("nestwal: too many effect headers")
		}
		keys := make([]string, 0, len(e.Headers))
		for key := range e.Headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_ = binary.Write(b, binary.BigEndian, uint32(len(keys)))
		for _, key := range keys {
			if err := writeString(b, key); err != nil {
				return err
			}
			if err := writeString(b, e.Headers[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeRecord(raw []byte) (corenest.CommitRecord, error) {
	r := bytes.NewReader(raw)
	var version uint16
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return corenest.CommitRecord{}, err
	}
	switch version {
	case codecVersionV1:
		return decodeRecordV1(r)
	case codecVersionV2:
		return decodeRecordV2(r)
	default:
		return corenest.CommitRecord{}, fmt.Errorf("%w: codec=%d", ErrUnsupportedRecordVersion, version)
	}
}

func decodeRecordHeader(r *bytes.Reader) (corenest.CommitRecord, error) {
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
	return record, nil
}

func decodeRecordV1(r *bytes.Reader) (corenest.CommitRecord, error) {
	record, err := decodeRecordHeader(r)
	if err != nil {
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
		canonical, canonicalErr := dataengine.CanonicalizeMutation(*m)
		if canonicalErr != nil {
			return record, canonicalErr
		}
		*m = canonical
	}
	if record.Effects, err = readEffects(r, false); err != nil {
		return record, err
	}
	if r.Len() != 0 {
		return record, fmt.Errorf("nestwal: %d trailing record bytes", r.Len())
	}
	if err := dataengine.ValidateCommitRecord(record); err != nil {
		return record, err
	}
	return record, nil
}

func decodeRecordV2(r *bytes.Reader) (corenest.CommitRecord, error) {
	record, err := decodeRecordHeader(r)
	if err != nil {
		return record, err
	}
	mutationCount, err := readCount(r)
	if err != nil {
		return record, err
	}
	record.Mutations = make([]corenest.EntityMutation, mutationCount)
	for i := range record.Mutations {
		m := &record.Mutations[i]
		if err := binary.Read(r, binary.BigEndian, &m.Key.ID); err != nil {
			return record, err
		}
		for _, field := range []any{&m.ExpectedVersion, &m.NextVersion, &m.Mask, &m.Schema} {
			if err := binary.Read(r, binary.BigEndian, field); err != nil {
				return record, err
			}
		}
		kind, readErr := r.ReadByte()
		if readErr != nil {
			return record, readErr
		}
		m.Kind = dataengine.MutationKind(kind)
		scope, readErr := r.ReadByte()
		if readErr != nil {
			return record, readErr
		}
		m.Key.Scope = dataengine.DatabaseScope(scope)
		if m.Key.Database, err = readString(r); err != nil {
			return record, err
		}
		if m.Key.Resource, err = readString(r); err != nil {
			return record, err
		}
		if m.Codec, err = readString(r); err != nil {
			return record, err
		}
		if m.Data, err = readBytes(r); err != nil {
			return record, err
		}
		if m.Patch.SetBSON, err = readBytes(r); err != nil {
			return record, err
		}
		unsetCount, readErr := readCount(r)
		if readErr != nil {
			return record, readErr
		}
		m.Patch.Unset = make([]string, unsetCount)
		for j := range m.Patch.Unset {
			if m.Patch.Unset[j], err = readString(r); err != nil {
				return record, err
			}
		}
		if m.Remote, err = readRemoteCommit(r); err != nil {
			return record, err
		}
	}
	if record.Effects, err = readEffects(r, true); err != nil {
		return record, err
	}
	receiptCount, err := readCount(r)
	if err != nil {
		return record, err
	}
	record.Receipts = make([]dataengine.Receipt, receiptCount)
	for i := range record.Receipts {
		receipt := &record.Receipts[i]
		if receipt.Namespace, err = readString(r); err != nil {
			return record, err
		}
		if receipt.ID, err = readString(r); err != nil {
			return record, err
		}
		if receipt.Digest, err = readBytes(r); err != nil {
			return record, err
		}
		if receipt.Payload, err = readBytes(r); err != nil {
			return record, err
		}
		if err := binary.Read(r, binary.BigEndian, &receipt.ExpiresAt); err != nil {
			return record, err
		}
	}
	if r.Len() != 0 {
		return record, fmt.Errorf("nestwal: %d trailing record bytes", r.Len())
	}
	if err := dataengine.ValidateCommitRecord(record); err != nil {
		return record, err
	}
	return record, nil
}

func readEffects(r *bytes.Reader, includeAvailableAt bool) ([]corenest.Effect, error) {
	effectCount, err := readCount(r)
	if err != nil {
		return nil, err
	}
	effects := make([]corenest.Effect, effectCount)
	for i := range effects {
		e := &effects[i]
		if e.ID, err = readString(r); err != nil {
			return nil, err
		}
		if e.Topic, err = readString(r); err != nil {
			return nil, err
		}
		if e.Key, err = readString(r); err != nil {
			return nil, err
		}
		if e.Payload, err = readBytes(r); err != nil {
			return nil, err
		}
		if includeAvailableAt {
			if err := binary.Read(r, binary.BigEndian, &e.AvailableAt); err != nil {
				return nil, err
			}
		}
		headerCount, readErr := readCount(r)
		if readErr != nil {
			return nil, readErr
		}
		if headerCount > 0 {
			e.Headers = make(map[string]string, headerCount)
		}
		for range headerCount {
			key, readErr := readString(r)
			if readErr != nil {
				return nil, readErr
			}
			value, readErr := readString(r)
			if readErr != nil {
				return nil, readErr
			}
			e.Headers[key] = value
		}
	}
	return effects, nil
}

func recordSizeHint(record corenest.CommitRecord) int {
	n := 64 + len(record.Handler) + len(record.RequestID)
	for i := range record.Mutations {
		m := &record.Mutations[i]
		n += 72 + len(m.Key.Database) + len(m.Key.Resource) + len(m.Codec) + len(m.Data) + len(m.Patch.SetBSON)
		for _, path := range m.Patch.Unset {
			n += 4 + len(path)
		}
	}
	for i := range record.Effects {
		e := &record.Effects[i]
		n += 32 + len(e.ID) + len(e.Topic) + len(e.Key) + len(e.Payload)
		for key, value := range e.Headers {
			n += 8 + len(key) + len(value)
		}
	}
	for i := range record.Receipts {
		receipt := &record.Receipts[i]
		n += 32 + len(receipt.Namespace) + len(receipt.ID) + len(receipt.Digest) + len(receipt.Payload)
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
