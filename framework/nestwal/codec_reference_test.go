// 编码参照实现对照：手写定长编码（codec.go 的 append*）必须与改造前基于 binary.Write 的实现逐字节相同，
// 并能被 decodeRecord 原样读回。参照实现原样保留自 v1.27.0 的 codec.go，只改了函数名；
// 它是 codec7 字节布局的独立见证，修改落盘格式时两边同时改并提升 codecVersion。

package nestwal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 随机记录覆盖 Put / Patch / Delete、Remote 更新与删除、负数有符号字段（补码）、空与非空的
// 字符串、字节、effect headers 与 receipts；每条都要求新旧编码逐字节相同且解码后再编码不变。
func TestEncodeMatchesBinaryWriteReference(t *testing.T) {
	samples := []corenest.CommitRecord{
		canonicalRecord(dataengine.MutationPut),
		canonicalRecord(dataengine.MutationPatch),
		canonicalRecord(dataengine.MutationDelete),
		testRecord(7, corenest.DurabilityStrict),
	}
	rng := rand.New(rand.NewPCG(20261015, 1027))
	for range 2000 {
		samples = append(samples, randomCodecRecord(t, rng))
	}
	for i, record := range samples {
		want, wantErr := legacyEncodeRecord(record)
		got, gotErr := encodeRecord(record)
		if wantErr != nil || gotErr != nil {
			t.Fatalf("sample %d: encode errors legacy=%v new=%v", i, wantErr, gotErr)
		}
		if !bytes.Equal(got, want) {
			at := 0
			for at < len(got) && at < len(want) && got[at] == want[at] {
				at++
			}
			t.Fatalf("sample %d: encoding differs at byte %d (new len=%d legacy len=%d)", i, at, len(got), len(want))
		}
		decoded, err := decodeRecord(got)
		if err != nil {
			t.Fatalf("sample %d: decode: %v", i, err)
		}
		again, err := encodeRecord(decoded)
		if err != nil || !bytes.Equal(again, got) {
			t.Fatalf("sample %d: re-encode after decode differs: err=%v", i, err)
		}
		if decoded.CreatedAt != record.CreatedAt || decoded.ID != record.ID || len(decoded.Mutations) != len(record.Mutations) {
			t.Fatalf("sample %d: decoded header/mutations mismatch", i)
		}
	}
}

// 编码失败的输入：新旧实现给出同一个错误，且新实现不返回半截 payload。
func TestEncodeRejectsSameInputsAsBinaryWriteReference(t *testing.T) {
	long := strings.Repeat("x", maxStringBytes+1)
	cases := map[string]func(*corenest.CommitRecord){
		"handler too long":    func(r *corenest.CommitRecord) { r.Handler = long },
		"request id too long": func(r *corenest.CommitRecord) { r.RequestID = long },
		"codec too long":      func(r *corenest.CommitRecord) { r.Mutations[0].Codec = long },
		"effect key too long": func(r *corenest.CommitRecord) { r.Effects[0].Key = long },
		"header too long":     func(r *corenest.CommitRecord) { r.Effects[0].Headers["a"] = long },
		"receipt id too long": func(r *corenest.CommitRecord) { r.Receipts[0].ID = long },
		"database too long":   func(r *corenest.CommitRecord) { r.Mutations[0].Key.Database = long },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			record := canonicalRecord(dataengine.MutationPut)
			mutate(&record)
			want, wantErr := legacyEncodeRecord(record)
			got, gotErr := encodeRecord(record)
			if wantErr == nil || gotErr == nil || wantErr.Error() != gotErr.Error() {
				t.Fatalf("errors differ: legacy=%v new=%v", wantErr, gotErr)
			}
			if got != nil || want != nil {
				t.Fatalf("payload returned with error: new=%d legacy=%d", len(got), len(want))
			}
		})
	}
}

var registerCodecReferenceKind sync.Once

const codecReferenceKind entity.EntityKind = 196

// randomCodecRecord 生成一条能通过 validateRecord 的随机记录。
func randomCodecRecord(t *testing.T, rng *rand.Rand) corenest.CommitRecord {
	t.Helper()
	var record corenest.CommitRecord
	for i := range record.ID {
		record.ID[i] = byte(rng.Uint32())
	}
	record.ID[15] |= 1
	record.CreatedAt = int64(rng.Uint64())
	record.Durability = dataengine.Durability(rng.IntN(int(dataengine.DurabilityPipelined) + 1))
	record.Handler = randomCodecString(rng, 24)
	record.RequestID = randomCodecString(rng, 24)
	if rng.IntN(5) == 0 {
		record.Mutations = []corenest.EntityMutation{randomRemoteMutation(t, rng)}
	} else {
		for range rng.IntN(4) {
			record.Mutations = append(record.Mutations, randomPlainMutation(t, rng))
		}
	}
	for range rng.IntN(3) {
		e := corenest.Effect{
			ID: "e" + randomCodecString(rng, 8), Topic: "t" + randomCodecString(rng, 8), Key: randomCodecString(rng, 8),
			Payload: randomCodecBytes(rng, 32), AvailableAt: int64(rng.Uint64()),
		}
		if n := rng.IntN(4); n > 0 {
			e.Headers = make(map[string]string, n)
			for range n {
				e.Headers[randomCodecString(rng, 6)] = randomCodecString(rng, 12)
			}
		}
		record.Effects = append(record.Effects, e)
	}
	for range rng.IntN(3) {
		record.Receipts = append(record.Receipts, dataengine.Receipt{
			Namespace: "ns" + randomCodecString(rng, 6), ID: "r" + randomCodecString(rng, 6),
			Digest: randomCodecBytes(rng, 16), Payload: randomCodecBytes(rng, 16), ExpiresAt: int64(rng.Uint64()),
		})
	}
	if record.Empty() {
		record.Receipts = []dataengine.Receipt{{Namespace: "ns", ID: "only"}}
	}
	if err := validateRecord(record); err != nil {
		t.Fatalf("generator produced invalid record: %v", err)
	}
	return record
}

func randomPlainMutation(t *testing.T, rng *rand.Rand) corenest.EntityMutation {
	t.Helper()
	m := corenest.EntityMutation{
		Key: dataengine.DocumentKey{
			Database: "db" + randomCodecString(rng, 6), Scope: dataengine.DatabaseScope(rng.IntN(256)),
			Resource: "res" + randomCodecString(rng, 6), ID: int64(rng.Uint64()) | 1,
		},
		Kind:   dataengine.MutationKind(rng.IntN(3) + 1),
		Mask:   rng.Uint64(),
		Schema: rng.Uint32(),
		Codec:  randomCodecString(rng, 8),
	}
	m.ExpectedVersion = rng.Uint64() >> 1
	if m.Kind == dataengine.MutationPatch && m.ExpectedVersion == 0 {
		m.ExpectedVersion = 1
	}
	m.NextVersion = m.ExpectedVersion + 1
	switch m.Kind {
	case dataengine.MutationPut:
		m.Data = append(randomCodecBytes(rng, 64), 0xff)
	case dataengine.MutationPatch:
		doc := bson.D{}
		for i := range rng.IntN(4) + 1 {
			doc = append(doc, bson.E{Key: "f" + strconv.Itoa(i), Value: int64(rng.Uint64())})
		}
		raw, err := bson.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		m.Patch.SetBSON = raw
		for i := range rng.IntN(3) {
			m.Patch.Unset = append(m.Patch.Unset, "u"+strconv.Itoa(i)+".x")
		}
	}
	return m
}

func randomRemoteMutation(t *testing.T, rng *rand.Rand) corenest.EntityMutation {
	t.Helper()
	registerCodecReferenceKind.Do(func() {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: codecReferenceKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	})
	entityID, err := entity.BuildEntityID(int64(rng.IntN(1<<20)+1), codecReferenceKind)
	if err != nil {
		t.Fatal(err)
	}
	base := rng.Uint64() >> 1
	commit := entity.RemoteCommit{
		EntityID: entityID, Kind: codecReferenceKind, Delete: rng.IntN(3) == 0,
		BaseVersion: base, NextVersion: base + 1, MarkerEpoch: rng.Uint64() | 1, LockFence: rng.Uint64(), RouteEpoch: rng.Uint64() | 1,
		Schema: rng.Uint32(), Codec: uint16(rng.Uint32()), Checksum: entity.RemoteChecksum(rng.Uint64()),
	}
	for i := range commit.TransactionID {
		commit.TransactionID[i] = byte(rng.Uint32())
	}
	commit.TransactionID[0] |= 1
	collections := []string{"players", "bags", "mail"}
	n := rng.IntN(len(collections)) + 1
	for i := range n {
		if commit.Delete {
			commit.Deletes = append(commit.Deletes, entity.RemoteDataDelete{Database: "game", DatabaseScope: uint8(rng.IntN(256)), Collection: collections[i], ID: entityID})
		} else {
			commit.Mutations = append(commit.Mutations, entity.RemoteDataMutation{
				Database: "game", DatabaseScope: uint8(rng.IntN(256)), Collection: collections[i], ID: entityID,
				Version: commit.NextVersion, Mask: rng.Uint64(), Data: append(randomCodecBytes(rng, 32), 1),
			})
		}
	}
	scopes := []uint32{rng.Uint32(), rng.Uint32() ^ 1}
	if rng.IntN(2) == 0 {
		data := append(randomCodecBytes(rng, 48), 2)
		checksum := entity.RemoteChecksum(0)
		if rng.IntN(2) == 0 {
			checksum = entity.RemoteSnapshotChecksum(data)
		}
		commit.Snapshots = []entity.RemoteSnapshotRecord{{
			Key:         entity.RemoteSnapshotKey{Tenant: rng.Uint32(), EntityID: entityID, Kind: codecReferenceKind, Scope: scopes[0], Policy: rng.Uint32()},
			BaseVersion: commit.BaseVersion, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, RouteEpoch: commit.RouteEpoch,
			Schema: rng.Uint32() | 1, Codec: uint16(rng.Uint32()), Full: rng.IntN(2) == 0, Data: data, Checksum: checksum,
		}}
	}
	if rng.IntN(2) == 0 {
		commit.Invalidations = []entity.RemoteSnapshotKey{{Tenant: rng.Uint32(), EntityID: entityID, Kind: codecReferenceKind, Scope: scopes[1], Policy: rng.Uint32()}}
	}
	kind := dataengine.MutationPut
	if commit.Delete {
		kind = dataengine.MutationDelete
	}
	return corenest.EntityMutation{
		Key:  dataengine.DocumentKey{ID: entityID, Resource: "remote_entity", Scope: dataengine.DatabaseScope(rng.IntN(256))},
		Kind: kind, ExpectedVersion: commit.BaseVersion, NextVersion: commit.NextVersion,
		Mask: rng.Uint64(), Schema: rng.Uint32(), Codec: "remote", Remote: &commit,
	}
}

func randomCodecString(rng *rand.Rand, max int) string {
	return string(randomCodecBytes(rng, max))
}

func randomCodecBytes(rng *rand.Rand, max int) []byte {
	n := rng.IntN(max + 1)
	if n == 0 && rng.IntN(2) == 0 {
		return nil
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

// 以下是 v1.27.0 codec.go 的编码实现（binary.Write），只做重命名，作为逐字节对照的参照。

func legacyEncodeRecordHeader(b *bytes.Buffer, record corenest.CommitRecord) error {
	_ = binary.Write(b, binary.BigEndian, codecVersion)
	_, _ = b.Write(record.ID[:])
	_ = binary.Write(b, binary.BigEndian, record.CreatedAt)
	_ = b.WriteByte(byte(record.Durability))
	if err := legacyWriteString(b, record.Handler); err != nil {
		return err
	}
	if err := legacyWriteString(b, record.RequestID); err != nil {
		return err
	}
	return nil
}

func legacyEncodeRecord(record corenest.CommitRecord) ([]byte, error) {
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	b := bytes.NewBuffer(make([]byte, 0, recordSizeHint(record)))
	if err := legacyEncodeRecordHeader(b, record); err != nil {
		return nil, err
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(record.Mutations)))
	for i := range record.Mutations {
		if err := legacyWriteMutation(b, &record.Mutations[i]); err != nil {
			return nil, err
		}
	}
	if err := legacyWriteEffects(b, record.Effects); err != nil {
		return nil, err
	}
	if err := legacyWriteReceipts(b, record.Receipts); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func legacyWriteMutation(b *bytes.Buffer, m *corenest.EntityMutation) error {
	_ = binary.Write(b, binary.BigEndian, m.Key.ID)
	_ = binary.Write(b, binary.BigEndian, m.ExpectedVersion)
	_ = binary.Write(b, binary.BigEndian, m.NextVersion)
	_ = binary.Write(b, binary.BigEndian, m.Mask)
	_ = binary.Write(b, binary.BigEndian, m.Schema)
	_ = b.WriteByte(byte(m.Kind))
	_ = b.WriteByte(byte(m.Key.Scope))
	if err := legacyWriteString(b, m.Key.Database); err != nil {
		return err
	}
	if err := legacyWriteString(b, m.Key.Resource); err != nil {
		return err
	}
	if err := legacyWriteString(b, m.Codec); err != nil {
		return err
	}
	if err := legacyWriteBytes(b, m.Data); err != nil {
		return err
	}
	if err := legacyWriteBytes(b, m.Patch.SetBSON); err != nil {
		return err
	}
	if len(m.Patch.Unset) > maxEntryCount {
		return errors.New("nestwal: too many unset paths")
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(m.Patch.Unset)))
	for _, path := range m.Patch.Unset {
		if err := legacyWriteString(b, path); err != nil {
			return err
		}
	}
	return legacyWriteRemoteCommit(b, m.Remote)
}

func legacyWriteReceipts(b *bytes.Buffer, receipts []dataengine.Receipt) error {
	_ = binary.Write(b, binary.BigEndian, uint32(len(receipts)))
	for i := range receipts {
		receipt := &receipts[i]
		if err := legacyWriteString(b, receipt.Namespace); err != nil {
			return err
		}
		if err := legacyWriteString(b, receipt.ID); err != nil {
			return err
		}
		if err := legacyWriteBytes(b, receipt.Digest); err != nil {
			return err
		}
		if err := legacyWriteBytes(b, receipt.Payload); err != nil {
			return err
		}
		_ = binary.Write(b, binary.BigEndian, receipt.ExpiresAt)
	}
	return nil
}

func legacyWriteEffects(b *bytes.Buffer, effects []corenest.Effect) error {
	_ = binary.Write(b, binary.BigEndian, uint32(len(effects)))
	for i := range effects {
		e := &effects[i]
		if err := legacyWriteString(b, e.ID); err != nil {
			return err
		}
		if err := legacyWriteString(b, e.Topic); err != nil {
			return err
		}
		if err := legacyWriteString(b, e.Key); err != nil {
			return err
		}
		if err := legacyWriteBytes(b, e.Payload); err != nil {
			return err
		}
		_ = binary.Write(b, binary.BigEndian, e.AvailableAt)
		if len(e.Headers) > maxEntryCount {
			return errors.New("nestwal: too many effect headers")
		}
		keys := make([]string, 0, len(e.Headers))
		for key := range e.Headers {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		_ = binary.Write(b, binary.BigEndian, uint32(len(keys)))
		for _, key := range keys {
			if err := legacyWriteString(b, key); err != nil {
				return err
			}
			if err := legacyWriteString(b, e.Headers[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

func legacyWriteString(w *bytes.Buffer, value string) error {
	if len(value) > maxStringBytes {
		return errors.New("nestwal: string exceeds limit")
	}
	return legacyWriteBytes(w, []byte(value))
}

func legacyWriteBytes(w *bytes.Buffer, value []byte) error {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return errors.New("nestwal: byte field exceeds uint32")
	}
	_ = binary.Write(w, binary.BigEndian, uint32(len(value)))
	_, _ = w.Write(value)
	return nil
}

func legacyWriteRemoteCommit(w *bytes.Buffer, commit *entity.RemoteCommit) error {
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
		if err := legacyWriteString(w, mutation.Database); err != nil {
			return err
		}
		_ = w.WriteByte(mutation.DatabaseScope)
		if err := legacyWriteString(w, mutation.Collection); err != nil {
			return err
		}
		_ = binary.Write(w, binary.BigEndian, mutation.ID)
		_ = binary.Write(w, binary.BigEndian, mutation.Version)
		_ = binary.Write(w, binary.BigEndian, mutation.Mask)
		if err := legacyWriteBytes(w, mutation.Data); err != nil {
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
		if err := legacyWriteString(w, item.Database); err != nil {
			return err
		}
		_ = w.WriteByte(item.DatabaseScope)
		if err := legacyWriteString(w, item.Collection); err != nil {
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
		if err := legacyWriteBytes(w, s.Data); err != nil {
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
