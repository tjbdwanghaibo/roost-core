package global

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RR-20261006-05（A7）：Bind 结果未知（写已落库、回复丢了）后，调用方用同样的参数重试，应当得到同一个绑定，
// 而不是 ErrConflict "already bound"。之前 !created 时直接报冲突，没有和已存的绑定比较；
// 只有已存的 group / globalSID 与请求不一致才是真正的冲突。

// lostReplyStore 让下一次 Create 照常落库、但把回复换成传输错误，模拟结果未知。
type lostReplyStore struct {
	versionstore.Store[int32, RouteBinding]
	loseNext bool
}

var errReplyLost = errors.New("connection reset after write")

func (s *lostReplyStore) Create(ctx context.Context, key int32, value RouteBinding) (versionstore.Versioned[RouteBinding], bool, error) {
	stored, created, err := s.Store.Create(ctx, key, value)
	if s.loseNext && err == nil {
		s.loseNext = false
		return versionstore.Versioned[RouteBinding]{}, false, errReplyLost
	}
	return stored, created, err
}

func TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	store := &lostReplyStore{Store: versionstore.NewMemoryStore[int32, RouteBinding](), loseNext: true}
	service, _ := newService(t, func(cfg *Config) {
		cfg.Routes = store
		cfg.Metrics = sink
	})
	ctx := context.Background()

	if _, err := service.Bind(ctx, 7, "group-a", 100); !errors.Is(err, errReplyLost) {
		t.Fatalf("first bind: err=%v, want the lost reply", err)
	}
	again, err := service.Bind(ctx, 7, "group-a", 100)
	if err != nil {
		t.Fatalf("retrying the same bind after an unknown outcome: %v, want the stored binding", err)
	}
	if again.GameSID != 7 || again.GlobalGroupID != "group-a" || again.GlobalSID != 100 || again.Epoch != 1 || again.State != RouteActive {
		t.Fatalf("retried bind returned %+v", again)
	}
	if got := sink.Count("replayed:bind"); got != 1 {
		t.Fatalf("a retried bind reported %d replays; %s", got, sink.Events())
	}
	if got := sink.Count("conflict:bind"); got != 0 {
		t.Fatalf("a retried bind reported %d conflicts; %s", got, sink.Events())
	}

	// 已存的绑定与请求不一致仍是冲突：换 group、换 globalSID 都不算重试。
	for _, tc := range []struct {
		group  string
		global int32
	}{{"group-b", 100}, {"group-a", 200}} {
		if _, err := service.Bind(ctx, 7, tc.group, tc.global); !errors.Is(err, ErrConflict) {
			t.Fatalf("bind %s/%d over group-a/100: err=%v, want ErrConflict", tc.group, tc.global, err)
		}
	}
	if got := sink.Count("conflict:bind"); got != 2 {
		t.Fatalf("mismatched binds reported %d conflicts; %s", got, sink.Events())
	}
	resolved, err := service.Resolve(ctx, 7)
	if err != nil || resolved.GlobalGroupID != "group-a" || resolved.GlobalSID != 100 || resolved.Epoch != 1 {
		t.Fatalf("binding after refused binds: %+v err=%v", resolved, err)
	}
}
