package skillsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

const fileOutboxVersion uint32 = 1
const fileOutboxMaxRecords = 1_000_000
const fileOutboxMaxRecordBytes int64 = 64 << 20

// PutRecord 的临时文件由 os.CreateTemp 按 “outbox-*.tmp” 生成，* 是十进制随机数。
const (
	fileOutboxTemporaryPrefix  = "outbox-"
	fileOutboxTemporarySuffix  = ".tmp"
	fileOutboxTemporaryPattern = fileOutboxTemporaryPrefix + "*" + fileOutboxTemporarySuffix
)

type fileOutboxEnvelope struct {
	Version  uint32       `json:"version"`
	Record   OutboxRecord `json:"record"`
	Checksum string       `json:"checksum"`
}

// FileOutboxStore persists one atomically replaceable file per network packet.
// This makes packet insertion and retry-metadata updates crash-safe without a
// database dependency.
//
// 目录归一个 store 独占：打开时会删掉崩溃遗留的 outbox-<数字>.tmp 临时文件，同一目录不能同时被
// 另一个在写的 store 使用（RR-20261006-04）。
type FileOutboxStore struct {
	mutex          sync.Mutex
	directory      string
	maxRecords     int
	maxRecordBytes int64
	recordCount    int
}

type FileOutboxOptions struct {
	MaxRecords     int
	MaxRecordBytes int64
}

func NewFileOutboxStore(directory string) (*FileOutboxStore, error) {
	return NewFileOutboxStoreWithOptions(directory, FileOutboxOptions{})
}

func NewFileOutboxStoreWithOptions(directory string, options FileOutboxOptions) (*FileOutboxStore, error) {
	if directory == "" {
		return nil, ErrOutboxStoreRequired
	}
	if options.MaxRecords <= 0 {
		options.MaxRecords = 100000
	} else if options.MaxRecords > fileOutboxMaxRecords {
		options.MaxRecords = fileOutboxMaxRecords
	}
	if options.MaxRecordBytes <= 0 {
		options.MaxRecordBytes = 16 << 20
	} else if options.MaxRecordBytes > fileOutboxMaxRecordBytes {
		options.MaxRecordBytes = fileOutboxMaxRecordBytes
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, err
	}
	count := 0
	removedTemporary := false
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 && filepath.Ext(entry.Name()) == ".packet" {
			return nil, ErrRecordInvalid
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".packet" {
			count++
		}
		// 写入中途崩溃会留下 PutRecord 的临时文件：它们从未替换成记录，内容不完整也不计入上限，
		// 只有打开时清理才不会每次崩溃泄漏一个（RR-20261006-04，O6）。目录归这一个 store 所有，
		// 打开时没有在途写入；只删确认是本 outbox 生成的普通文件，其他文件一律不动。
		if isFileOutboxTemporary(entry) {
			if err := os.Remove(filepath.Join(absolute, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			removedTemporary = true
		}
	}
	if count > options.MaxRecords {
		return nil, ErrOutboxStoreLimit
	}
	if removedTemporary {
		if err := syncDirectory(absolute); err != nil {
			return nil, err
		}
	}
	return &FileOutboxStore{directory: absolute, maxRecords: options.MaxRecords, maxRecordBytes: options.MaxRecordBytes, recordCount: count}, nil
}

func (store *FileOutboxStore) Load() ([]syncstream.Packet, error) {
	records, err := store.LoadRecords()
	if err != nil {
		return nil, err
	}
	packets := make([]syncstream.Packet, len(records))
	for index := range records {
		packets[index] = records[index].Packet.Clone()
	}
	return packets, nil
}

func (store *FileOutboxStore) LoadRecords() ([]OutboxRecord, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, err
	}
	records := make([]OutboxRecord, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".packet" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, ErrRecordInvalid
		}
		if len(records) >= store.maxRecords {
			return nil, ErrOutboxStoreLimit
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Size() < 0 || info.Size() > store.maxRecordBytes {
			return nil, ErrOutboxStoreLimit
		}
		data, err := readOutboxFile(filepath.Join(store.directory, entry.Name()), store.maxRecordBytes)
		if err != nil {
			return nil, err
		}
		record, err := decodeFileOutboxRecord(data)
		if err != nil {
			return nil, err
		}
		if outboxFilename(record.Packet.Observer, record.Packet.Stream, record.Packet.Epoch, record.Packet.Sequence) != entry.Name() {
			return nil, ErrRecordInvalid
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Packet.Observer != records[j].Packet.Observer {
			return observerLess(records[i].Packet.Observer, records[j].Packet.Observer)
		}
		if records[i].Packet.Stream.Topic != records[j].Packet.Stream.Topic {
			return records[i].Packet.Stream.Topic < records[j].Packet.Stream.Topic
		}
		if records[i].Packet.Stream.Key != records[j].Packet.Stream.Key {
			return records[i].Packet.Stream.Key < records[j].Packet.Stream.Key
		}
		if records[i].Packet.Epoch != records[j].Packet.Epoch {
			return records[i].Packet.Epoch < records[j].Packet.Epoch
		}
		return records[i].Packet.Sequence < records[j].Packet.Sequence
	})
	store.recordCount = len(records)
	return records, nil
}

func (store *FileOutboxStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet.Clone(), CreatedAt: time.Now()})
}

func (store *FileOutboxStore) PutRecord(record OutboxRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	recordData, err := json.Marshal(record)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(recordData)
	data, err := json.Marshal(fileOutboxEnvelope{Version: fileOutboxVersion, Record: record, Checksum: hex.EncodeToString(digest[:])})
	if err != nil {
		return err
	}
	if int64(len(data)) > store.maxRecordBytes {
		return ErrOutboxStoreLimit
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	packet := record.Packet
	if packet.Stream.Topic == "" || packet.Epoch == 0 || packet.Sequence == 0 {
		return ErrRecordInvalid
	}
	target := filepath.Join(store.directory, outboxFilename(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence))
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if !exists && store.recordCount >= store.maxRecords {
		return ErrOutboxStoreLimit
	}
	temporary, err := os.CreateTemp(store.directory, fileOutboxTemporaryPattern)
	if err != nil {
		return err
	}
	name := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(name, target); err != nil {
		return err
	}
	remove = false
	if !exists {
		store.recordCount++
	}
	return syncDirectory(store.directory)
}

func decodeFileOutboxRecord(data []byte) (OutboxRecord, error) {
	var envelope fileOutboxEnvelope
	if err := decodeStrict(data, &envelope); err != nil || envelope.Version != fileOutboxVersion {
		return OutboxRecord{}, ErrRecordInvalid
	}
	recordData, err := json.Marshal(envelope.Record)
	if err != nil {
		return OutboxRecord{}, err
	}
	digest := sha256.Sum256(recordData)
	if !bytes.Equal([]byte(envelope.Checksum), []byte(hex.EncodeToString(digest[:]))) {
		return OutboxRecord{}, ErrRecordInvalid
	}
	return envelope.Record, nil
}

func (store *FileOutboxStore) Delete(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	err := os.Remove(filepath.Join(store.directory, outboxFilename(observer, stream, epoch, sequence)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if store.recordCount > 0 {
		store.recordCount--
	}
	return syncDirectory(store.directory)
}

// isFileOutboxTemporary 只认 os.CreateTemp(dir, fileOutboxTemporaryPattern) 生成的名字：
// “outbox-” + 十进制数 + “.tmp”，且是普通文件（不跟随符号链接、不碰目录）。
func isFileOutboxTemporary(entry os.DirEntry) bool {
	if !entry.Type().IsRegular() {
		return false
	}
	name := entry.Name()
	digits, ok := strings.CutPrefix(name, fileOutboxTemporaryPrefix)
	if !ok {
		return false
	}
	digits, ok = strings.CutSuffix(digits, fileOutboxTemporarySuffix)
	if !ok || digits == "" {
		return false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func readOutboxFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, ErrOutboxStoreLimit
	}
	return data, nil
}

func outboxFilename(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) string {
	data, _ := json.Marshal(struct {
		Observer syncstream.Observer
		Stream   syncstream.Stream
		Epoch    uint64
		Sequence uint64
	}{observer, stream, epoch, sequence})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + ".packet"
}
