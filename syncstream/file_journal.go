package syncstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// FileHistoryJournal stores immutable checkpoint/WAL generations. A
// checkpoint first creates and fsyncs the next WAL, then atomically publishes
// a uniquely named checkpoint. Recovery requires the newest pair to be valid,
// so corruption is never hidden by silently rolling authoritative state back.
// The previous generation remains available for explicit operator recovery.
type FileHistoryJournal struct {
	mutex        sync.Mutex
	directory    string
	initialEpoch uint64
	generation   uint64
	// walFile is the resident append handle for the current generation,
	// opened lazily on first Record and dropped on generation rotation.
	walFile *os.File
	// Group commit: concurrent Records append to pending and one leader
	// drains it with a single write+fsync — "Record returns => durable"
	// stays intact while fsync count drops from per-record to per-batch.
	batchMu   sync.Mutex
	pending   []byte
	waiters   []chan error
	flushing  bool
	idle      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
	// truncateTo, when >= 0, is the byte length of the last complete record
	// in the current WAL as found by Load. Recovery ignores an unterminated
	// tail (it was never committed), but the bytes are still on disk and
	// O_APPEND would glue the next record onto them, producing a line the
	// next recovery cannot decode (RR-20260915-07). The first append after
	// recovery truncates to this offset under the journal's exclusive
	// ownership, then syncs, then appends (U-0211).
	truncateTo int64
	// failure is set when a write or checkpoint publish ended with an
	// indeterminate outcome. See ErrHistoryJournalFailed (U-0212).
	failure error
	// syncFile / publish are the durability primitives, held as fields so a
	// test can make them report failure AFTER the real operation succeeded —
	// the only way to exercise "side effect done, caller told otherwise".
	syncFile func(*os.File) error
	publish  func(from, to, directory string) error
}

type fileCheckpoint struct {
	Generation uint64          `json:"generation"`
	Snapshot   HistorySnapshot `json:"snapshot"`
}

func NewFileHistoryJournal(directory string, initialEpoch uint64) (*FileHistoryJournal, error) {
	if directory == "" {
		return nil, ErrHistoryJournalRequired
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	if err := syncJournalDirectory(absolute); err != nil {
		return nil, fmt.Errorf("syncstream: sync journal directory: %w", err)
	}
	if initialEpoch == 0 {
		initialEpoch = newEpoch()
	}
	return &FileHistoryJournal{
		directory: absolute, initialEpoch: initialEpoch, generation: 1, truncateTo: -1,
		syncFile: (*os.File).Sync, publish: durableReplace,
	}, nil
}

// failed reports the indeterminate-outcome state, wrapping its cause.
func (journal *FileHistoryJournal) failed() error {
	if journal.failure == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrHistoryJournalFailed, journal.failure)
}

func (journal *FileHistoryJournal) checkpointPath(generation uint64) string {
	return filepath.Join(journal.directory, fmt.Sprintf("history.checkpoint-%020d.json", generation))
}

func (journal *FileHistoryJournal) walPath(generation uint64) string {
	return filepath.Join(journal.directory, fmt.Sprintf("history.wal-%020d.jsonl", generation))
}

func (journal *FileHistoryJournal) Load() (HistorySnapshot, error) {
	if journal == nil || journal.closed.Load() {
		return HistorySnapshot{}, ErrHistoryJournalClosed
	}
	journal.mutex.Lock()
	defer journal.mutex.Unlock()

	generations, err := journal.checkpointGenerations()
	if err != nil {
		return HistorySnapshot{}, err
	}
	journal.truncateTo = -1
	if len(generations) == 0 {
		journal.generation = 1
		snapshot := HistorySnapshot{Version: HistorySnapshotVersion, Epoch: journal.initialEpoch}
		if info, statErr := os.Stat(journal.walPath(1)); statErr == nil {
			if info.Size() > 0 {
				// The first committed mutation is the durable source of the
				// randomly generated epoch before the first checkpoint.
				snapshot.Epoch = 0
			}
			valid, replayErr := replayWAL(journal.walPath(1), &snapshot)
			if replayErr != nil {
				return HistorySnapshot{}, replayErr
			}
			if valid < info.Size() {
				journal.truncateTo = valid
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return HistorySnapshot{}, statErr
		}
		return snapshot, nil
	}

	generation := generations[0]
	snapshot, valid, err := journal.loadGeneration(generation)
	if err != nil {
		return HistorySnapshot{}, fmt.Errorf("syncstream: newest history generation %d is invalid: %w", generation, err)
	}
	journal.generation = generation
	if info, statErr := os.Stat(journal.walPath(generation)); statErr == nil && valid < info.Size() {
		journal.truncateTo = valid
	}
	return snapshot, nil
}

func (journal *FileHistoryJournal) checkpointGenerations() ([]uint64, error) {
	paths, err := filepath.Glob(filepath.Join(journal.directory, "history.checkpoint-*.json"))
	if err != nil {
		return nil, err
	}
	result := make([]uint64, 0, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		value := strings.TrimSuffix(strings.TrimPrefix(name, "history.checkpoint-"), ".json")
		generation, parseErr := strconv.ParseUint(value, 10, 64)
		if parseErr == nil && generation > 0 {
			result = append(result, generation)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] > result[j] })
	return result, nil
}

func (journal *FileHistoryJournal) loadGeneration(generation uint64) (HistorySnapshot, int64, error) {
	data, err := os.ReadFile(journal.checkpointPath(generation))
	if err != nil {
		return HistorySnapshot{}, 0, err
	}
	var checkpoint fileCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return HistorySnapshot{}, 0, err
	}
	if checkpoint.Generation != generation {
		return HistorySnapshot{}, 0, ErrInvalidSnapshot
	}
	valid, err := replayWAL(journal.walPath(generation), &checkpoint.Snapshot)
	if err != nil {
		return HistorySnapshot{}, 0, err
	}
	return checkpoint.Snapshot, valid, nil
}

// replayWAL applies every complete record and returns the byte length of the
// file up to and including the last complete record. An unterminated tail
// was never durably committed and is skipped; a complete line that does not
// decode is corruption and is refused.
func replayWAL(path string, snapshot *HistorySnapshot) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("syncstream: WAL generation missing: %w", err)
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var valid int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && (readErr == nil || line[len(line)-1] == '\n') {
			var mutation HistoryMutation
			if err := json.Unmarshal(line, &mutation); err != nil {
				return valid, fmt.Errorf("syncstream: decode WAL: %w", err)
			}
			if err := replayHistoryMutation(snapshot, mutation); err != nil {
				return valid, err
			}
			valid += int64(len(line))
		}
		if errors.Is(readErr, io.EOF) {
			return valid, nil // an unterminated tail was never durably committed
		}
		if readErr != nil {
			return valid, readErr
		}
	}
}

func (journal *FileHistoryJournal) Record(mutation HistoryMutation) error {
	if journal == nil || journal.closed.Load() {
		return ErrHistoryJournalClosed
	}
	if mutation.Version != HistoryMutationVersion {
		return ErrInvalidSnapshot
	}
	journal.mutex.Lock()
	failed := journal.failed()
	journal.mutex.Unlock()
	if failed != nil {
		return failed
	}
	data, err := json.Marshal(mutation)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	journal.batchMu.Lock()
	if journal.closed.Load() {
		journal.batchMu.Unlock()
		return ErrHistoryJournalClosed
	}
	journal.pending = append(journal.pending, data...)
	done := make(chan error, 1)
	journal.waiters = append(journal.waiters, done)
	if journal.flushing {
		// A leader is draining; it will flush this batch too.
		journal.batchMu.Unlock()
		return <-done
	}
	journal.flushing = true
	journal.idle = make(chan struct{})
	journal.batchMu.Unlock()
	for {
		journal.batchMu.Lock()
		batch, waiters := journal.pending, journal.waiters
		journal.pending, journal.waiters = nil, nil
		if len(waiters) == 0 {
			journal.flushing = false
			close(journal.idle)
			journal.idle = nil
			journal.batchMu.Unlock()
			break
		}
		journal.batchMu.Unlock()
		flushErr := journal.flushBatch(batch)
		for _, waiter := range waiters {
			waiter <- flushErr
		}
	}
	return <-done
}

// flushBatch appends one batch to the current generation's WAL and fsyncs
// once. journal.mutex serializes it against Checkpoint's generation rotation.
func (journal *FileHistoryJournal) flushBatch(batch []byte) error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	if failed := journal.failed(); failed != nil {
		return failed
	}
	if journal.walFile == nil {
		path := journal.walPath(journal.generation)
		_, statErr := os.Stat(path)
		created := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !created {
			return statErr
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if created {
			if err := syncJournalDirectory(journal.directory); err != nil {
				_ = file.Close()
				return err
			}
		}
		if journal.truncateTo >= 0 {
			// Drop the unterminated tail recovery skipped before anything is
			// appended behind it (U-0211). Truncate works on the descriptor
			// regardless of O_APPEND; the next write lands at the new end.
			if err := file.Truncate(journal.truncateTo); err != nil {
				_ = file.Close()
				return err
			}
			if err := journal.syncFile(file); err != nil {
				_ = file.Close()
				return err
			}
			journal.truncateTo = -1
		}
		journal.walFile = file
	}
	// From the first byte written the outcome is indeterminate on error:
	// part of the batch may be durable, all of it, or none. The in-memory
	// History rolled its mutation back, so retrying would write the same
	// sequence twice and the next Load would refuse the WAL (RR-20260916-01).
	// Stop the journal instead; a reopen replays what is actually there.
	if _, err := journal.walFile.Write(batch); err != nil {
		journal.failure = err
		return journal.failed()
	}
	if err := journal.syncFile(journal.walFile); err != nil {
		journal.failure = err
		return journal.failed()
	}
	return nil
}

func (journal *FileHistoryJournal) Checkpoint(snapshot HistorySnapshot) error {
	if journal == nil || journal.closed.Load() {
		return ErrHistoryJournalClosed
	}
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	if failed := journal.failed(); failed != nil {
		return failed
	}

	next := journal.generation + 1
	wal, err := os.OpenFile(journal.walPath(next), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		// An orphan from a crash before checkpoint publication contains no
		// committed records and is safe to replace.
		if removeErr := os.Remove(journal.walPath(next)); removeErr != nil {
			return removeErr
		}
		wal, err = os.OpenFile(journal.walPath(next), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return err
	}
	if err := wal.Sync(); err != nil {
		_ = wal.Close()
		return err
	}
	if err := wal.Close(); err != nil {
		return err
	}
	if err := syncJournalDirectory(journal.directory); err != nil {
		return fmt.Errorf("syncstream: persist WAL generation %d: %w", next, err)
	}

	data, err := json.Marshal(fileCheckpoint{Generation: next, Snapshot: snapshot})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(journal.directory, "history.checkpoint-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
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
	// Everything above happens on files nothing reads yet and is safe to
	// retry. Publication is the one step whose failure is ambiguous: on
	// Linux the rename may have landed with only the directory sync failing,
	// so the new generation could already be the one the next Load picks
	// while this process still appends to the old WAL. Stop here too
	// (RR-20260916-01, U-0212).
	if err := journal.publish(temporaryName, journal.checkpointPath(next), journal.directory); err != nil {
		journal.failure = err
		removeTemporary = false // the temporary may already be the checkpoint
		return journal.failed()
	}
	removeTemporary = false
	journal.generation = next
	journal.truncateTo = -1
	if journal.walFile != nil {
		_ = journal.walFile.Close()
		journal.walFile = nil
	}
	journal.removeObsoleteGenerations(next)
	return nil
}

// Close waits for an admitted group-commit batch, then closes the resident WAL
// handle. It is idempotent. A closed journal rejects Load, Record, and
// Checkpoint so callers cannot silently fall back to non-durable history.
func (journal *FileHistoryJournal) Close() error {
	if journal == nil {
		return nil
	}
	journal.closeOnce.Do(func() {
		journal.closed.Store(true)
		journal.batchMu.Lock()
		idle := journal.idle
		journal.batchMu.Unlock()
		if idle != nil {
			<-idle
		}

		journal.mutex.Lock()
		defer journal.mutex.Unlock()
		if journal.walFile != nil {
			journal.closeErr = journal.walFile.Close()
			journal.walFile = nil
		}
	})
	return journal.closeErr
}

func (journal *FileHistoryJournal) removeObsoleteGenerations(current uint64) {
	if current <= 2 {
		return
	}
	obsolete := current - 2
	removed := false
	if err := os.Remove(journal.checkpointPath(obsolete)); err == nil {
		removed = true
	}
	if err := os.Remove(journal.walPath(obsolete)); err == nil {
		removed = true
	}
	if removed {
		_ = syncJournalDirectory(journal.directory)
	}
}

func replayHistoryMutation(snapshot *HistorySnapshot, mutation HistoryMutation) error {
	if snapshot == nil || mutation.Version != HistoryMutationVersion {
		return ErrInvalidSnapshot
	}
	if mutation.Kind == HistoryMutationRotateEpoch {
		snapshot.Epoch, snapshot.Streams, snapshot.SequenceFloor = mutation.Epoch, nil, 0
		return nil
	}
	if snapshot.Epoch == 0 {
		snapshot.Epoch = mutation.Epoch
	}
	if mutation.Epoch != snapshot.Epoch {
		return ErrInvalidSnapshot
	}
	find := func(observer Observer, stream Stream) int {
		for index := range snapshot.Streams {
			if snapshot.Streams[index].Observer == observer && snapshot.Streams[index].Stream == stream {
				return index
			}
		}
		return -1
	}
	switch mutation.Kind {
	case HistoryMutationAppend:
		index := find(mutation.Packet.Observer, mutation.Packet.Stream)
		if index < 0 {
			snapshot.Streams = append(snapshot.Streams, HistoryStreamSnapshot{Observer: mutation.Packet.Observer, Stream: mutation.Packet.Stream})
			index = len(snapshot.Streams) - 1
		}
		value := &snapshot.Streams[index]
		// A stream's first record fixes where it starts (a re-created
		// identity continues above the epoch's sequence floor, U-0215);
		// every later record must be exactly the next sequence.
		if value.Latest != 0 || len(value.Packets) > 0 {
			if mutation.Packet.Sequence != value.Latest+1 {
				return ErrInvalidSnapshot
			}
		} else if mutation.Packet.Sequence == 0 {
			return ErrInvalidSnapshot
		} else {
			// First record of a (re-)created stream: everything below it
			// belongs to the epoch's floor, not to this stream (mirrors
			// appendLocked's acked = floor).
			value.Acked = mutation.Packet.Sequence - 1
		}
		value.Latest = mutation.Packet.Sequence
		if mutation.Packet.Sequence > snapshot.SequenceFloor {
			snapshot.SequenceFloor = mutation.Packet.Sequence
		}
		value.Schema = mutation.Packet.SchemaVersion
		value.Packets = append(value.Packets, mutation.Packet.Clone())
	case HistoryMutationAcknowledge:
		index := find(mutation.Observer, mutation.Stream)
		if index < 0 || mutation.Sequence > snapshot.Streams[index].Latest {
			return ErrInvalidSnapshot
		}
		if mutation.Sequence > snapshot.Streams[index].Acked {
			value := &snapshot.Streams[index]
			value.Acked = mutation.Sequence
			if mutation.PruneAcknowledged {
				pruned := 0
				for pruned < len(value.Packets) && value.Packets[pruned].Sequence <= mutation.Sequence {
					pruned++
				}
				value.Packets = append([]Packet(nil), value.Packets[pruned:]...)
				value.Pruned += uint64(pruned)
			}
		}
	case HistoryMutationDelete:
		index := find(mutation.Observer, mutation.Stream)
		if index >= 0 {
			snapshot.Streams = append(snapshot.Streams[:index], snapshot.Streams[index+1:]...)
		}
	case HistoryMutationDeleteView:
		result := snapshot.Streams[:0]
		for _, value := range snapshot.Streams {
			if value.Observer != mutation.Observer {
				result = append(result, value)
			}
		}
		snapshot.Streams = result
	case HistoryMutationSweep:
		for _, target := range mutation.Targets {
			index := find(target.Observer, target.Stream)
			if index >= 0 {
				snapshot.Streams = append(snapshot.Streams[:index], snapshot.Streams[index+1:]...)
			}
		}
	default:
		return ErrInvalidSnapshot
	}
	return nil
}
