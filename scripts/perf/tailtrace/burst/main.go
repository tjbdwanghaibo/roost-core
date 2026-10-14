package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/trace"
)

// burst 是 tailtrace 的补充：看单个 tail-NN.trace 里第一簇长尾附近发生了什么。
//
//	go run ./burst <trace> <ms>            第一条 roost.tail 前 <ms> 到该簇结束，与 2s 前同长窗口对照：
//	                                       goroutine 创建、唤醒（按唤醒方栈）与可运行等待时间
//	go run ./burst <trace> <ms> spans      首条长尾前后 10ms 桶：goroutine 运行时间与 P 运行时间；最长运行区间
//	go run ./burst <trace> <ms> who        簇内 [-40ms,+85ms] 与 [-300ms,-175ms] 按 goroutine 身份（栈底帧）对照运行时间
//	go run ./burst x wall <trace>...       用 trace 的时钟快照把每个文件第一条长尾换算成墙钟时间
//
// 运行时间来自 trace 的状态区间：线程被宿主 OS 调走时 goroutine 仍记为 Running，所以“运行时间变长而
// 进程 rusage CPU 不变”是宿主抢占的旁证，不是直接证据。
func main() {
	path := os.Args[1]
	if os.Args[2] == "wall" {
		for _, p := range os.Args[3:] {
			wallOfFirstTail(p)
		}
		return
	}
	win, _ := strconv.Atoi(os.Args[2]) // ms window before first tail log
	var logs []int64
	read(path, func(e trace.Event) {
		if e.Kind() == trace.EventLog && e.Log().Category == "roost.tail" {
			logs = append(logs, int64(e.Time()))
		}
	})
	sort.Slice(logs, func(i, j int) bool { return logs[i] < logs[j] })
	first := logs[0]
	burstEnd := first
	for _, t := range logs {
		if t-burstEnd < 1e9 {
			burstEnd = t
		}
	}
	type acc struct {
		creates, unblocks map[string]int
		runnableNS        map[string]int64
	}
	mk := func() *acc { return &acc{map[string]int{}, map[string]int{}, map[string]int64{}} }
	A, B := mk(), mk()
	lo := first - int64(win)*1e6
	bLo, bHi := lo-2e9, first-2e9
	startFn := map[trace.GoID]string{}
	runnableSince := map[trace.GoID]int64{}
	read(path, func(e trace.Event) {
		if e.Kind() != trace.EventStateTransition {
			return
		}
		st := e.StateTransition()
		if st.Resource.Kind != trace.ResourceGoroutine {
			return
		}
		g := st.Resource.Goroutine()
		from, to := st.Goroutine()
		t := int64(e.Time())
		if from == trace.GoNotExist || from == trace.GoUndetermined {
			startFn[g] = top(st.Stack, 3)
		}
		var a *acc
		switch {
		case t >= lo && t <= burstEnd:
			a = A
		case t >= bLo && t <= bLo+(burstEnd-lo):
			a = B
		}
		_ = bHi
		if from == trace.GoRunnable {
			if s, ok := runnableSince[g]; ok && a != nil {
				a.runnableNS[startFn[g]] += t - s
			}
			delete(runnableSince, g)
		}
		if to == trace.GoRunnable {
			runnableSince[g] = t
		}
		if a == nil {
			return
		}
		if from == trace.GoNotExist && to == trace.GoRunnable {
			a.creates[top(st.Stack, 3)]++
		}
		if from == trace.GoWaiting && to == trace.GoRunnable {
			a.unblocks[top(e.Stack(), 4)]++
		}
	})
	if len(os.Args) > 3 && os.Args[3] == "who" {
		whoRuns(path, first, first-40e6, first+85e6, first-300e6, first-175e6)
		return
	}
	if len(os.Args) > 3 {
		runSpans(path, first)
		return
	}
	fmt.Printf("burst window: %.1fms before first tail log .. last tail log (+%.1fms); comparison window same length 2s earlier\n", float64(win), float64(burstEnd-first)/1e6)
	for _, kv := range []struct {
		name string
		a, b map[string]int
	}{{"goroutine creations by start", A.creates, B.creates}, {"unblocks by unblocker stack", A.unblocks, B.unblocks}} {
		fmt.Println("==", kv.name)
		printTop(kv.a, kv.b, 15)
	}
	fmt.Println("== runnable time (ms) by goroutine start function")
	ra, rb := map[string]int{}, map[string]int{}
	for k, v := range A.runnableNS {
		ra[k] = int(v / 1e6)
	}
	for k, v := range B.runnableNS {
		rb[k] = int(v / 1e6)
	}
	printTop(ra, rb, 15)
}

func printTop(a, b map[string]int, n int) {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return a[keys[i]] > a[keys[j]] })
	for i, k := range keys {
		if i >= n {
			break
		}
		fmt.Printf("  burst=%7d before=%7d  %s\n", a[k], b[k], k)
	}
}

func top(s trace.Stack, n int) string {
	var parts []string
	for f := range s.Frames() {
		fn := f.Func
		if strings.HasPrefix(fn, "runtime.") && len(parts) == 0 {
			continue
		}
		fn = strings.TrimPrefix(fn, "github.com/tjbdwanghaibo/roost-core/")
		parts = append(parts, fn)
		if len(parts) >= n {
			break
		}
	}
	return strings.Join(parts, " < ")
}

func read(path string, fn func(trace.Event)) {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r, err := trace.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		panic(err)
	}
	for {
		e, err := r.ReadEvent()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			panic(err)
		}
		fn(e)
	}
}

// runSpans: per 10ms bucket around the first tail log: total Running ns across goroutines, count of
// running spans, and longest Running spans (with the stack where they stopped running).
func runSpans(path string, first int64) {
	type span struct {
		g          trace.GoID
		start, end int64
		stack      string
	}
	running := map[trace.GoID]int64{}
	var long []span
	buckets := map[int64][2]int64{}
	lo, hi := first-1500e6, first+300e6
	procRun := map[trace.ProcID]int64{}
	procBuckets := map[int64]int64{}
	add := func(b map[int64][2]int64, s, e int64) {
		for t := s; t < e; {
			k := t / 10e6
			end := min(e, (k+1)*10e6)
			v := b[k]
			v[0] += end - t
			b[k] = v
			t = end
		}
	}
	read(path, func(ev trace.Event) {
		if ev.Kind() != trace.EventStateTransition {
			return
		}
		st := ev.StateTransition()
		t := int64(ev.Time())
		if st.Resource.Kind == trace.ResourceProc {
			p := st.Resource.Proc()
			_, to := st.Proc()
			if s, ok := procRun[p]; ok && to != trace.ProcRunning {
				if t > lo && s < hi {
					for x := max(s, lo); x < min(t, hi); {
						k := x / 10e6
						e := min(min(t, hi), (k+1)*10e6)
						procBuckets[k] += e - x
						x = e
					}
				}
				delete(procRun, p)
			}
			if to == trace.ProcRunning {
				procRun[p] = t
			}
			return
		}
		if st.Resource.Kind != trace.ResourceGoroutine {
			return
		}
		g := st.Resource.Goroutine()
		from, to := st.Goroutine()
		if from == trace.GoRunning {
			if s, ok := running[g]; ok && t > lo && s < hi {
				add(buckets, max(s, lo), min(t, hi))
				if t-s > 3e6 {
					long = append(long, span{g, s, t, top(ev.Stack(), 5)})
				}
			}
			delete(running, g)
		}
		if to == trace.GoRunning {
			running[g] = t
		}
	})
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	fmt.Println("10ms buckets relative to first tail log: goroutine-running ms / proc-running ms")
	for _, k := range keys {
		fmt.Printf("%+6dms  g-run=%6.2f  p-run=%6.2f\n", (k*10e6-first)/1e6, float64(buckets[k][0])/1e6, float64(procBuckets[k])/1e6)
	}
	sort.Slice(long, func(i, j int) bool { return long[i].end-long[i].start > long[j].end-long[j].start })
	fmt.Println("longest running spans (>3ms) in window:")
	for i, s := range long {
		if i >= 15 {
			break
		}
		fmt.Printf("  g=%d at=%+.1fms dur=%.2fms stop-stack=%s\n", s.g, float64(s.start-first)/1e6, float64(s.end-s.start)/1e6, s.stack)
	}
}

// whoRuns: running ns per goroutine identity (bottom frames of any stack seen for that goroutine) in a burst
// window vs an equally long normal window.
func whoRuns(path string, first int64, bLo, bHi, nLo, nHi int64) {
	ident := map[trace.GoID]string{}
	read(path, func(ev trace.Event) {
		g := ev.Goroutine()
		if g == trace.NoGoroutine {
			return
		}
		if _, ok := ident[g]; ok {
			return
		}
		var frames []string
		for f := range ev.Stack().Frames() {
			frames = append(frames, strings.TrimPrefix(f.Func, "github.com/tjbdwanghaibo/roost-core/"))
		}
		if len(frames) == 0 {
			return
		}
		n := len(frames)
		ident[g] = strings.Join(frames[max(0, n-3):], " > ")
	})
	running := map[trace.GoID]int64{}
	burst, normal := map[string]int64{}, map[string]int64{}
	acc := func(m map[string]int64, g trace.GoID, s, e, lo, hi int64) {
		if a, b := max(s, lo), min(e, hi); b > a {
			id := ident[g]
			if id == "" {
				id = "<no stack: runtime/GC worker>"
			}
			m[id] += b - a
		}
	}
	read(path, func(ev trace.Event) {
		if ev.Kind() != trace.EventStateTransition {
			return
		}
		st := ev.StateTransition()
		if st.Resource.Kind != trace.ResourceGoroutine {
			return
		}
		g := st.Resource.Goroutine()
		from, to := st.Goroutine()
		t := int64(ev.Time())
		if from == trace.GoRunning {
			if s, ok := running[g]; ok {
				acc(burst, g, s, t, bLo, bHi)
				acc(normal, g, s, t, nLo, nHi)
			}
			delete(running, g)
		}
		if to == trace.GoRunning {
			running[g] = t
		}
	})
	keys := make([]string, 0, len(burst))
	for k := range burst {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return burst[keys[i]]-normal[keys[i]] > burst[keys[j]]-normal[keys[j]] })
	var tb, tn int64
	for _, v := range burst {
		tb += v
	}
	for _, v := range normal {
		tn += v
	}
	fmt.Printf("total running: burst=%.1fms normal=%.1fms (windows %.0fms)\n", float64(tb)/1e6, float64(tn)/1e6, float64(bHi-bLo)/1e6)
	for i, k := range keys {
		if i >= 20 {
			break
		}
		fmt.Printf("  burst=%7.2fms normal=%7.2fms  %s\n", float64(burst[k])/1e6, float64(normal[k])/1e6, k)
	}
}

func wallOfFirstTail(path string) {
	var snapT trace.Time
	var snapWall time.Time
	var first trace.Time = -1
	read(path, func(ev trace.Event) {
		if ev.Kind() == trace.EventSync {
			if cs := ev.Sync().ClockSnapshot; cs != nil && snapWall.IsZero() {
				snapT, snapWall = cs.Trace, cs.Wall
			}
		}
		if first < 0 && ev.Kind() == trace.EventLog && ev.Log().Category == "roost.tail" {
			first = ev.Time()
		}
	})
	w := snapWall.Add(time.Duration(first - snapT))
	fmt.Printf("%s first tail log wall=%s\n", path[len(path)-13:], w.Format("15:04:05.000"))
}
