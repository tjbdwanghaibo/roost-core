package syncstream

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// checkpointedJournal returns a closed journal directory holding generation 2
// (one checkpoint) plus one WAL record after it, and the generation number.
func checkpointedJournal(t *testing.T) (string, uint64) {
	t.Helper()
	directory := t.TempDir()
	journal, err := NewFileHistoryJournal(directory, 91)
	if err != nil {
		t.Fatal(err)
	}
	history, err := NewHistoryWithJournal(HistoryOptions{MaxPacketsPerStream: 8}, journal)
	if err != nil {
		t.Fatal(err)
	}
	observer, stream := Observer{ID: 4}, Stream{Topic: "state"}
	if _, err := history.Append(Packet{Observer: observer, Stream: stream, Full: true, Payload: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if err := history.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(Packet{Observer: observer, Stream: stream, Payload: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return directory, journal.generation
}

// The journal is the durable memory of every observer's stream position.
// A checkpoint whose body claims another generation, a generation whose WAL
// is gone, or a WAL line that does not decode must fail closed on Load —
// replaying a wrong or partial history would hand clients a resync they
// cannot reconcile.
func TestFileHistoryJournalLoadFailsClosedOnEachIntegrityDefect(t *testing.T) {
	t.Run("checkpoint claims another generation", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		raw, err := os.ReadFile(probe.checkpointPath(generation))
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint fileCheckpoint
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			t.Fatal(err)
		}
		checkpoint.Generation++
		forged, _ := json.Marshal(checkpoint)
		if err := os.WriteFile(probe.checkpointPath(generation), forged, 0o600); err != nil {
			t.Fatal(err)
		}
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		if _, err := journal.Load(); !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("Load with a forged generation = %v, want ErrInvalidSnapshot", err)
		}
	})
	t.Run("WAL of the newest generation is missing", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		if err := os.Remove(probe.walPath(generation)); err != nil {
			t.Fatal(err)
		}
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		_, err = journal.Load()
		if err == nil || !strings.Contains(err.Error(), "WAL generation missing") {
			t.Fatalf("Load without the WAL = %v", err)
		}
	})
	t.Run("WAL line does not decode", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		file, err := os.OpenFile(probe.walPath(generation), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("{not json}\n"); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		_, err = journal.Load()
		if err == nil || !strings.Contains(err.Error(), "decode WAL") {
			t.Fatalf("Load with a corrupt WAL line = %v", err)
		}
	})
}

// A closed journal accepts nothing, and a mutation from another wire version
// is refused before it reaches the WAL.
func TestFileHistoryJournalRefusesRecordsAfterCloseAndFromOtherVersions(t *testing.T) {
	journal, err := NewFileHistoryJournal(t.TempDir(), 91)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(HistoryMutation{Version: HistoryMutationVersion + 1}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Record with a foreign version = %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(HistoryMutation{Version: HistoryMutationVersion}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Record after Close = %v", err)
	}
	if _, err := journal.Load(); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Load after Close = %v", err)
	}
	if err := journal.Checkpoint(HistorySnapshot{}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Checkpoint after Close = %v", err)
	}
}
