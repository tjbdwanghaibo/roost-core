package log

import (
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"testing"
	"time"
)

func TestServerTimeUsesSystemClockInsideBusinessContext(t *testing.T) {
	ctx, release := fctx.NewContext()
	defer release()
	ctx.NowMilli = time.Now().Add(24 * time.Hour).UnixMilli()
	before := time.Now().UnixMilli()
	attrs := (contextHandler{}).contextAttrs(0)
	after := time.Now().UnixMilli()
	for _, a := range attrs {
		if a.Key == "server_time_ms" {
			if n := a.Value.Int64(); n < before || n > after {
				t.Fatalf("server_time_ms=%d follows business offset; system window=[%d,%d]", n, before, after)
			}
			return
		}
	}
	t.Fatal("missing server time")
}
