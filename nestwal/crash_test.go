package nestwal

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

const crashChildDirEnv = "NESTWAL_CRASH_DIR"

func crashRecord(sequence uint64) corenest.CommitRecord {
	var id corenest.TransactionID
	binary.BigEndian.PutUint64(id[8:], sequence)
	return corenest.CommitRecord{
		ID:         id,
		Handler:    "crash.handler",
		RequestID:  "crash",
		CreatedAt:  time.Now().UnixNano(),
		Durability: corenest.DurabilityPipelined,
		Mutations: []corenest.EntityMutation{{
			EntityID: int64(sequence),
			Database: "game",
			Resource: "players",
			Version:  sequence,
			Codec:    "bson",
			Data:     []byte{byte(sequence), 1, 2, 3},
		}},
	}
}

func crashRecordSequence(record corenest.CommitRecord) uint64 {
	return binary.BigEndian.Uint64(record.ID[8:])
}

// TestNestWALCrashChildProcess is the re-executed child of the crash test
// below. It enqueues pipelined records at a steady pace, reports each ticket
// resolution on stdout, and keeps going until the parent kills it with
// SIGKILL mid-stream.
func TestNestWALCrashChildProcess(t *testing.T) {
	dir := os.Getenv(crashChildDirEnv)
	if dir == "" {
		t.Skip("crash child helper; driven by TestNestWALCrashKeepsDurablePrefix")
	}
	w, err := Open(testOptions(dir))
	if err != nil {
		fmt.Printf("OPEN_ERROR %v\n", err)
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 100000; sequence++ {
		ticket, err := w.Enqueue(context.Background(), crashRecord(sequence))
		if err != nil {
			fmt.Printf("ENQUEUE_ERROR %d %v\n", sequence, err)
			t.Fatal(err)
		}
		go func() {
			<-ticket.Done()
			if ticket.Err() == nil {
				fmt.Printf("DURABLE %d\n", ticket.LSN())
			}
		}()
		time.Sleep(200 * time.Microsecond)
	}
	// Unreachable in a healthy run: the parent kills the process long before
	// 100000 records.
	select {}
}

// TestNestWALCrashKeepsDurablePrefix is the design-doc §8.3 crash test: the
// child process is killed with SIGKILL between enqueue and fsync of later
// records, and a reopened WAL must replay a contiguous prefix that covers
// every ticket the child reported durable. A resolved ticket is a durability
// promise; a gap or a missing reported record would break every
// externalization gated on it.
func TestNestWALCrashKeepsDurablePrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestNestWALCrashChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), crashChildDirEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// Kill only after enough tickets resolved that the child is provably
	// mid-stream: records keep being enqueued while we deliver SIGKILL.
	const killAfterDurable = 20
	maxReportedDurable := uint64(0)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if seq, ok := strings.CutPrefix(line, "DURABLE "); ok {
			lsn, err := strconv.ParseUint(seq, 10, 64)
			if err != nil {
				t.Fatalf("bad child line %q: %v", line, err)
			}
			if lsn > maxReportedDurable {
				maxReportedDurable = lsn
			}
			if maxReportedDurable >= killAfterDurable {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "OPEN_ERROR") || strings.HasPrefix(line, "ENQUEUE_ERROR") {
			t.Fatalf("child failed: %s", line)
		}
	}
	if maxReportedDurable < killAfterDurable {
		t.Fatalf("child exited after only %d durable tickets", maxReportedDurable)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	// The kernel released the child's writer lock with the process; reopen
	// and replay. Torn-tail recovery may truncate unsynced bytes — never a
	// record whose ticket resolved.
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	next := uint64(1)
	if err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		sequence := crashRecordSequence(record)
		if sequence != next {
			return fmt.Errorf("replay is not a contiguous prefix: got %d, want %d", sequence, next)
		}
		next++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recovered := next - 1
	if recovered < maxReportedDurable {
		t.Fatalf("durability promise broken: child reported LSN %d durable, replay recovered only %d records", maxReportedDurable, recovered)
	}
	t.Logf("crash recovery: reported durable=%d, recovered prefix=%d", maxReportedDurable, recovered)
}
