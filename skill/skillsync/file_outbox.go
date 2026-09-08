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
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

const fileOutboxVersion uint32 = 1
const fileOutboxMaxRecords = 1_000_000
const fileOutboxMaxRecordBytes int64 = 64 << 20

type fileOutboxEnvelope struct {
	Version  uint32       `json:"version"`
	Record   OutboxRecord `json:"record"`
	Checksum string       `json:"checksum"`
}

// FileOutboxStore persists one atomically replaceable file per network packet.
// This makes packet insertion and retry-metadata updates crash-safe without a
// database dependency.
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
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 && filepath.Ext(entry.Name()) == ".packet" {
			return nil, ErrRecordInvalid
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".packet" {
			count++
		}
	}
	if count > options.MaxRecords {
		return nil, ErrOutboxStoreLimit
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
	temporary, err := os.CreateTemp(store.directory, "outbox-*.tmp")
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
