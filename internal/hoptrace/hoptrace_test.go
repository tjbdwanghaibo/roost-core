package hoptrace

import (
	"sync"
	"testing"
	"time"
)

type memoryRecorder struct {
	mu     sync.Mutex
	values map[string][]time.Duration
	marks  map[string]time.Time
}

func (m *memoryRecorder) Observe(hop string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[hop] = append(m.values[hop], d)
}

func (m *memoryRecorder) Mark(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marks[key] = at
}

func (m *memoryRecorder) Take(key string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.marks[key]
	delete(m.marks, key)
	return at, ok
}

// 没安装 Recorder 时计时点不读时钟、不分配（生产路径的承诺）：键的拼接在确认安装之后才发生。
func TestUninstalledTracePointsDoNotAllocate(t *testing.T) {
	Install(nil)
	id := "command-1"
	allocs := testing.AllocsPerRun(1000, func() {
		start := Now()
		Since("hop", start)
		Mark("published/", id)
		SinceMark("hop", "published/", id)
		SinceMarkAt("hop", "published/", id, start)
		Observe("hop", time.Millisecond)
		if !start.IsZero() || Enabled() {
			t.Fatal("trace point active without a recorder")
		}
	})
	if allocs != 0 {
		t.Fatalf("uninstalled trace points allocate %.1f times per call", allocs)
	}
}

// 安装后 Mark / SinceMark 按对象配对、取走即删；Now 为零值时 Since 不记。
func TestInstalledRecorderPairsMarksOnce(t *testing.T) {
	recorder := &memoryRecorder{values: map[string][]time.Duration{}, marks: map[string]time.Time{}}
	Install(recorder)
	t.Cleanup(func() { Install(nil) })
	Mark("published/", "a")
	SinceMark("deliver", "published/", "a")
	SinceMark("deliver", "published/", "a") // 已取走，不再记
	SinceMark("deliver", "published/", "b") // 从没标记
	Since("zero", time.Time{})
	start := Now()
	Since("timed", start)
	if got := len(recorder.values["deliver"]); got != 1 {
		t.Fatalf("deliver observed %d times, want 1", got)
	}
	if len(recorder.values["zero"]) != 0 || len(recorder.values["timed"]) != 1 || len(recorder.marks) != 0 {
		t.Fatalf("values=%v marks=%v", recorder.values, recorder.marks)
	}
}
