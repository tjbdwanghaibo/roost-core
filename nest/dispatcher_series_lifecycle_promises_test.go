package nest

// RR-20261006-18：派发器的 nest.dispatch.*{dispatcher} 序列在派发器销毁后一直留在注册表里，停在最后的值
// （第十二轮 O3）。承诺：派发器排空停止（OnDestroyWithContext 返回 nil）后删掉带它 dispatcher 标签的全部
// 序列；创建再销毁 N 次序列数不增长，/metrics 里不再出现已销毁的派发器；同名派发器还有活着的
// （生产里 NestMgr 都叫 "nest"）时不删，留给最后一个；停机超时（返回 ctx 错误）时不删，重试排空后再删。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

const seriesLifecycleDispatcherPrefix = "rr18-series-"

// seriesByDispatcher counts the series carrying a dispatcher label this test
// owns, by dispatcher name.
func seriesByDispatcher() map[string]int {
	out := map[string]int{}
	for _, metric := range metrics.Snapshot() {
		if name := metric.Labels["dispatcher"]; strings.HasPrefix(name, seriesLifecycleDispatcherPrefix) {
			out[name]++
		}
	}
	return out
}

func startObservedDispatcher(t *testing.T, name string) *Dispatcher {
	t.Helper()
	d := NewDispatcher(name, 1, 0, 8, func(*Msg) {})
	d.OnInit()
	d.OnRun()
	d.observeStats()
	return d
}

func TestDestroyedDispatchersLeaveNoSeries(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	const rounds = 20
	counts := make([]int, 0, rounds)
	for i := range rounds {
		name := fmt.Sprintf("%s%d", seriesLifecycleDispatcherPrefix, i)
		d := startObservedDispatcher(t, name)
		if got := seriesByDispatcher()[name]; got == 0 {
			t.Fatalf("round %d: the live dispatcher %s reported no series", i, name)
		}
		if err := d.OnDestroyWithContext(context.Background()); err != nil {
			t.Fatalf("round %d: destroy: %v", i, err)
		}
		live := seriesByDispatcher()
		total := 0
		for _, n := range live {
			total += n
		}
		counts = append(counts, total)
	}
	for _, n := range counts {
		if n != 0 {
			t.Fatalf("series of destroyed dispatchers after each create/destroy round: %v; want 0 every round", counts)
		}
	}
	if text := string(metrics.PrometheusText(metrics.Snapshot())); strings.Contains(text, seriesLifecycleDispatcherPrefix) {
		t.Fatalf("/metrics still shows destroyed dispatchers:\n%s", text)
	}
}

func TestADispatcherSharingItsNameKeepsTheSeries(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	name := seriesLifecycleDispatcherPrefix + "shared"
	first := startObservedDispatcher(t, name)
	second := startObservedDispatcher(t, name)
	if err := first.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seriesByDispatcher()[name] == 0 {
		t.Fatalf("destroying one of two live dispatchers named %s deleted the series the other still reports", name)
	}
	if err := second.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := seriesByDispatcher()[name]; got != 0 {
		t.Fatalf("the last dispatcher named %s is destroyed and %d series remain", name, got)
	}
}

func TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	name := seriesLifecycleDispatcherPrefix + "stuck"
	release := make(chan struct{})
	started := make(chan struct{})
	d := NewDispatcher(name, 1, 0, 8, func(*Msg) {
		close(started)
		<-release
	})
	d.OnInit()
	d.OnRun()
	if err := d.TrySendMsg(GenMsg(MsgTypeSingle)); err != nil {
		t.Fatal(err)
	}
	<-started
	d.observeStats()

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.OnDestroyWithContext(expired); err == nil {
		t.Fatal("destroy returned nil while a handler was still running")
	}
	if seriesByDispatcher()[name] == 0 {
		t.Fatal("a dispatcher whose stop timed out deleted its series; its workers are still running")
	}
	close(release)
	if err := d.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("retried destroy: %v", err)
	}
	if got := seriesByDispatcher()[name]; got != 0 {
		t.Fatalf("the drained dispatcher still has %d series", got)
	}
}
