package nestwal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

const (
	// codecVersion 是唯一支持的落盘格式；旧格式明确拒绝，不做在线转换。
	codecVersion   = uint16(7)
	maxEntryCount  = 1 << 20
	maxStringBytes = 1 << 20
)

// ErrUnsupportedRecordVersion 表示记录的编码版本不是 codecVersion；旧格式需用匹配的旧程序处理。
var ErrUnsupportedRecordVersion = errors.New("nestwal: unsupported record version")

// validateRecord 在编码前检查记录：非空、持久级别合法、条目数不超限，并通过 DataEngine 的记录校验。
func validateRecord(record corenest.CommitRecord) error {
	if record.Empty() {
		return errors.New("nestwal: empty commit record")
	}
	if record.Durability > dataengine.DurabilityPipelined {
		return errors.New("nestwal: invalid durability policy")
	}
	if len(record.Mutations) > maxEntryCount || len(record.Effects) > maxEntryCount || len(record.Receipts) > maxEntryCount {
		return errors.New("nestwal: too many entries in commit record")
	}
	return dataengine.ValidateCommitRecord(record)
}

// appendRecordHeader 追加编码版本与记录头（ID、创建时间、持久级别、handler、请求 ID）。
//
// 编码侧全部用 binary.BigEndian.AppendUintXX 手写定长字段：编码在 Enqueue 同步完成，
// 调用方此时持有实体锁，binary.Write 的反射与接口装箱会让每个定长字段多一次分配。字节序与字段宽度必须与
// decodeRecord 侧的 binary.Read 逐字节一致（有符号整数按补码写入同宽无符号数）；
// codec_reference_test.go 用改造前的 binary.Write 实现做逐字节对照。
func appendRecordHeader(b []byte, record corenest.CommitRecord) ([]byte, error) {
	b = binary.BigEndian.AppendUint16(b, codecVersion)
	b = append(b, record.ID[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(record.CreatedAt))
	b = append(b, byte(record.Durability))
	var err error
	if b, err = appendString(b, record.Handler); err != nil {
		return nil, err
	}
	return appendString(b, record.RequestID)
}

// encodeRecord 把提交记录编码为 WAL payload，顺序为：记录头、mutations、effects、receipts。
// 字段顺序即落盘格式，修改须提升 codecVersion。
func encodeRecord(record corenest.CommitRecord) ([]byte, error) {
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	b, err := appendRecordHeader(make([]byte, 0, recordSizeHint(record)), record)
	if err != nil {
		return nil, err
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(record.Mutations)))
	for i := range record.Mutations {
		if b, err = appendMutation(b, &record.Mutations[i]); err != nil {
			return nil, err
		}
	}
	if b, err = appendEffects(b, record.Effects); err != nil {
		return nil, err
	}
	return appendReceipts(b, record.Receipts)
}

// appendMutation 编码一条实体变更，含可选的 Remote 提交。
func appendMutation(b []byte, m *corenest.EntityMutation) ([]byte, error) {
	b = binary.BigEndian.AppendUint64(b, uint64(m.Key.ID))
	b = binary.BigEndian.AppendUint64(b, m.ExpectedVersion)
	b = binary.BigEndian.AppendUint64(b, m.NextVersion)
	b = binary.BigEndian.AppendUint64(b, m.Mask)
	b = binary.BigEndian.AppendUint32(b, m.Schema)
	b = append(b, byte(m.Kind), byte(m.Key.Scope))
	var err error
	for _, s := range [...]string{m.Key.Database, m.Key.Resource, m.Codec} {
		if b, err = appendString(b, s); err != nil {
			return nil, err
		}
	}
	if b, err = appendBytes(b, m.Data); err != nil {
		return nil, err
	}
	if b, err = appendBytes(b, m.Patch.SetBSON); err != nil {
		return nil, err
	}
	if len(m.Patch.Unset) > maxEntryCount {
		return nil, errors.New("nestwal: too many unset paths")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(m.Patch.Unset)))
	for _, path := range m.Patch.Unset {
		if b, err = appendString(b, path); err != nil {
			return nil, err
		}
	}
	return appendRemoteCommit(b, m.Remote)
}

// appendReceipts 编码幂等回执列表。
func appendReceipts(b []byte, receipts []dataengine.Receipt) ([]byte, error) {
	b = binary.BigEndian.AppendUint32(b, uint32(len(receipts)))
	var err error
	for i := range receipts {
		receipt := &receipts[i]
		if b, err = appendString(b, receipt.Namespace); err != nil {
			return nil, err
		}
		if b, err = appendString(b, receipt.ID); err != nil {
			return nil, err
		}
		if b, err = appendBytes(b, receipt.Digest); err != nil {
			return nil, err
		}
		if b, err = appendBytes(b, receipt.Payload); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint64(b, uint64(receipt.ExpiresAt))
	}
	return b, nil
}

// appendEffects 编码外部效果列表；Headers 按键排序，保证同一记录编码结果确定。
func appendEffects(b []byte, effects []corenest.Effect) ([]byte, error) {
	b = binary.BigEndian.AppendUint32(b, uint32(len(effects)))
	var err error
	for i := range effects {
		e := &effects[i]
		for _, s := range [...]string{e.ID, e.Topic, e.Key} {
			if b, err = appendString(b, s); err != nil {
				return nil, err
			}
		}
		if b, err = appendBytes(b, e.Payload); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint64(b, uint64(e.AvailableAt))
		if len(e.Headers) > maxEntryCount {
			return nil, errors.New("nestwal: too many effect headers")
		}
		keys := make([]string, 0, len(e.Headers))
		for key := range e.Headers {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		b = binary.BigEndian.AppendUint32(b, uint32(len(keys)))
		for _, key := range keys {
			if b, err = appendString(b, key); err != nil {
				return nil, err
			}
			if b, err = appendString(b, e.Headers[key]); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

// decodeRecord 解码一条 WAL payload；版本不符返回 ErrUnsupportedRecordVersion，有尾随字节或
// 解码结果未通过 DataEngine 校验时报错。
func decodeRecord(raw []byte) (corenest.CommitRecord, error) {
	r := bytes.NewReader(raw)
	var version uint16
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return corenest.CommitRecord{}, err
	}
	if version != codecVersion {
		return corenest.CommitRecord{}, fmt.Errorf("%w: codec=%d", ErrUnsupportedRecordVersion, version)
	}
	return decodeRecordBody(r)
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
	record.Durability = dataengine.Durability(durability)
	if record.Handler, err = readString(r); err != nil {
		return record, err
	}
	if record.RequestID, err = readString(r); err != nil {
		return record, err
	}
	return record, nil
}

func decodeRecordBody(r *bytes.Reader) (corenest.CommitRecord, error) {
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
		if err := readMutation(r, &record.Mutations[i]); err != nil {
			return record, err
		}
	}
	if record.Effects, err = readEffects(r); err != nil {
		return record, err
	}
	if record.Receipts, err = readReceipts(r); err != nil {
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

// readMutation 解码一条实体变更，与 appendMutation 对称。
func readMutation(r *bytes.Reader, m *corenest.EntityMutation) error {
	var err error
	if err := binary.Read(r, binary.BigEndian, &m.Key.ID); err != nil {
		return err
	}
	for _, field := range []any{&m.ExpectedVersion, &m.NextVersion, &m.Mask, &m.Schema} {
		if err := binary.Read(r, binary.BigEndian, field); err != nil {
			return err
		}
	}
	kind, err := r.ReadByte()
	if err != nil {
		return err
	}
	m.Kind = dataengine.MutationKind(kind)
	scope, err := r.ReadByte()
	if err != nil {
		return err
	}
	m.Key.Scope = dataengine.DatabaseScope(scope)
	if m.Key.Database, err = readString(r); err != nil {
		return err
	}
	if m.Key.Resource, err = readString(r); err != nil {
		return err
	}
	if m.Codec, err = readString(r); err != nil {
		return err
	}
	if m.Data, err = readBytes(r); err != nil {
		return err
	}
	if m.Patch.SetBSON, err = readBytes(r); err != nil {
		return err
	}
	unsetCount, err := readCount(r)
	if err != nil {
		return err
	}
	m.Patch.Unset = make([]string, unsetCount)
	for j := range m.Patch.Unset {
		if m.Patch.Unset[j], err = readString(r); err != nil {
			return err
		}
	}
	m.Remote, err = readRemoteCommit(r)
	return err
}

// readReceipts 解码幂等回执列表，与 appendReceipts 对称。
func readReceipts(r *bytes.Reader) ([]dataengine.Receipt, error) {
	receiptCount, err := readCount(r)
	if err != nil {
		return nil, err
	}
	receipts := make([]dataengine.Receipt, receiptCount)
	for i := range receipts {
		receipt := &receipts[i]
		if receipt.Namespace, err = readString(r); err != nil {
			return nil, err
		}
		if receipt.ID, err = readString(r); err != nil {
			return nil, err
		}
		if receipt.Digest, err = readBytes(r); err != nil {
			return nil, err
		}
		if receipt.Payload, err = readBytes(r); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.BigEndian, &receipt.ExpiresAt); err != nil {
			return nil, err
		}
	}
	return receipts, nil
}

func readEffects(r *bytes.Reader) ([]corenest.Effect, error) {
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
		if err := binary.Read(r, binary.BigEndian, &e.AvailableAt); err != nil {
			return nil, err
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

// recordSizeHint 估算编码长度，用于一次性预分配缓冲区；只影响分配次数，不影响格式。
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

func appendString(b []byte, value string) ([]byte, error) {
	if len(value) > maxStringBytes {
		return nil, errors.New("nestwal: string exceeds limit")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(value)))
	return append(b, value...), nil
}

func appendBytes(b []byte, value []byte) ([]byte, error) {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return nil, errors.New("nestwal: byte field exceeds uint32")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(value)))
	return append(b, value...), nil
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

// appendRemoteCommit 编码可选的 Remote 提交：先写 0/1 存在标记，存在时写入校验后的完整提交。
func appendRemoteCommit(b []byte, commit *entity.RemoteCommit) ([]byte, error) {
	if commit == nil {
		return append(b, 0), nil
	}
	if err := commit.Validate(); err != nil {
		return nil, err
	}
	b = append(b, 1)
	b = append(b, commit.TransactionID[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(commit.EntityID))
	b = binary.BigEndian.AppendUint16(b, uint16(commit.Kind))
	b = append(b, boolByte(commit.Delete))
	b = binary.BigEndian.AppendUint64(b, commit.BaseVersion)
	b = binary.BigEndian.AppendUint64(b, commit.NextVersion)
	b = binary.BigEndian.AppendUint64(b, commit.MarkerEpoch)
	b = binary.BigEndian.AppendUint64(b, commit.LockFence)
	b = binary.BigEndian.AppendUint64(b, commit.RouteEpoch)
	b = binary.BigEndian.AppendUint32(b, commit.Schema)
	b = binary.BigEndian.AppendUint16(b, commit.Codec)
	b = binary.BigEndian.AppendUint64(b, uint64(commit.Checksum))
	if len(commit.Mutations) > maxEntryCount {
		return nil, errors.New("nestwal: remote mutation count exceeds limit")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(commit.Mutations)))
	var err error
	for i := range commit.Mutations {
		mutation := &commit.Mutations[i]
		if err := mutation.Validate(); err != nil {
			return nil, err
		}
		if b, err = appendString(b, mutation.Database); err != nil {
			return nil, err
		}
		b = append(b, mutation.DatabaseScope)
		if b, err = appendString(b, mutation.Collection); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint64(b, uint64(mutation.ID))
		b = binary.BigEndian.AppendUint64(b, mutation.Version)
		b = binary.BigEndian.AppendUint64(b, mutation.Mask)
		if b, err = appendBytes(b, mutation.Data); err != nil {
			return nil, err
		}
	}
	if len(commit.Deletes) > maxEntryCount {
		return nil, errors.New("nestwal: remote delete count exceeds limit")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(commit.Deletes)))
	for i := range commit.Deletes {
		item := &commit.Deletes[i]
		if err := item.Validate(); err != nil {
			return nil, err
		}
		if b, err = appendString(b, item.Database); err != nil {
			return nil, err
		}
		b = append(b, item.DatabaseScope)
		if b, err = appendString(b, item.Collection); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint64(b, uint64(item.ID))
	}
	if len(commit.Snapshots) > maxEntryCount {
		return nil, errors.New("nestwal: remote snapshot count exceeds limit")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(commit.Snapshots)))
	for i := range commit.Snapshots {
		s := &commit.Snapshots[i]
		b = appendRemoteSnapshotKey(b, s.Key)
		b = binary.BigEndian.AppendUint64(b, s.BaseVersion)
		b = binary.BigEndian.AppendUint64(b, s.StateVersion)
		b = binary.BigEndian.AppendUint64(b, s.MarkerEpoch)
		b = binary.BigEndian.AppendUint64(b, s.RouteEpoch)
		b = binary.BigEndian.AppendUint32(b, s.Schema)
		b = binary.BigEndian.AppendUint16(b, s.Codec)
		b = append(b, boolByte(s.Full))
		b = binary.BigEndian.AppendUint64(b, uint64(s.Checksum))
		if b, err = appendBytes(b, s.Data); err != nil {
			return nil, err
		}
	}
	if len(commit.Invalidations) > maxEntryCount {
		return nil, errors.New("nestwal: remote invalidation count exceeds limit")
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(commit.Invalidations)))
	for _, key := range commit.Invalidations {
		b = appendRemoteSnapshotKey(b, key)
	}
	return b, nil
}

// appendRemoteSnapshotKey 编码快照键：Tenant u32、EntityID i64、Kind u16、Scope u32、Policy u32。
func appendRemoteSnapshotKey(b []byte, key entity.RemoteSnapshotKey) []byte {
	b = binary.BigEndian.AppendUint32(b, key.Tenant)
	b = binary.BigEndian.AppendUint64(b, uint64(key.EntityID))
	b = binary.BigEndian.AppendUint16(b, uint16(key.Kind))
	b = binary.BigEndian.AppendUint32(b, key.Scope)
	return binary.BigEndian.AppendUint32(b, key.Policy)
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
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
