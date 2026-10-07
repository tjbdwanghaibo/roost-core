package failurelog

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"testing"
)

type trimResultRedis struct {
	*fakeRedis
	result any
}

func (r trimResultRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return r.result, nil
}
func TestLuaAppendCountsTrimmedRecords(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	l := NewRedisList(trimResultRedis{newFakeRedis(), int64(5)}, Config{Namespace: "b7_trim", MaxEntries: 2})
	if err := l.AppendRaw(context.Background(), "b7", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, s := range metrics.Snapshot() {
		if s.Name == "failurelog_trim_total" && s.Labels["namespace"] == "b7_trim" && s.Value == 3 {
			return
		}
	}
	t.Fatal("Lua removed three records without the trim counter")
}
