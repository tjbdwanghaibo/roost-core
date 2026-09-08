package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0104 · C2（空洞测试）· B-18。
//
// SubscriptionCoordinator 的三个公开入口（Subscribe / Unsubscribe / FlushSubject）
// 和信封汇的两处 nil 守卫决定了"坏参数在门口被拒绝"这条承诺：一个空的
// SubscriberRef 不能悄悄变成一条以零值为键的订阅；主体 ID 为 0 的状态不能
// 进入按主体分片的操作；没有可靠信封汇时不能宣称订阅已激活。这些守卫此前
// 没有任何测试触达（gap map 7/9 无覆盖），去掉它们后包测试仍然全绿。
//
// 每个用例都保证被测守卫是唯一可能的拒绝者：其余参数全部合法，
// 协调器处于打开状态并带有可用的信封汇。

func newPromiseCoordinator(t *testing.T) (*SubscriptionCoordinator, *entity.SubjectSyncState, SubscriberRef) {
	t.Helper()
	packCount := 0
	state := newSubscriptionTestState(t, 7001, &packCount)
	coordinator := NewSubscriptionCoordinator(&recordingEnvelopeSink{})
	t.Cleanup(coordinator.Close)
	return coordinator, state, SubscriberRef{Kind: SubscriberKindPlayer, ID: 71}
}

func newZeroSubjectState(t *testing.T) *entity.SubjectSyncState {
	t.Helper()
	packCount := 0
	state := newSubscriptionTestState(t, 0, &packCount)
	if state == nil || state.SubjectID() != 0 {
		t.Fatalf("fixture: want an enabled state with subject 0, got %v", state)
	}
	return state
}

func TestSubscribeRejectsInvalidSubscriberAndSubject(t *testing.T) {
	coordinator, state, subscriber := newPromiseCoordinator(t)
	profile := entity.SyncProfile{Key: "default"}

	// 合法参数先成功，证明夹具本身不会被别的规则拒绝。
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, profile); err != nil {
		t.Fatalf("baseline Subscribe failed: %v", err)
	}
	if err := coordinator.Unsubscribe(context.Background(), subscriber, state.SubjectID()); err != nil {
		t.Fatalf("baseline Unsubscribe failed: %v", err)
	}

	cases := []struct {
		name        string
		coordinator *SubscriptionCoordinator
		subscriber  SubscriberRef
		state       *entity.SubjectSyncState
		wantErr     error
	}{
		{name: "nil coordinator", coordinator: nil, subscriber: subscriber, state: state, wantErr: ErrSubscriberInvalid},
		{name: "empty subscriber", coordinator: coordinator, subscriber: SubscriberRef{}, state: state, wantErr: ErrSubscriberInvalid},
		{name: "kind without identity", coordinator: coordinator, subscriber: SubscriberRef{Kind: SubscriberKindServer}, state: state, wantErr: ErrSubscriberInvalid},
		{name: "nil state", coordinator: coordinator, subscriber: subscriber, state: nil, wantErr: ErrSubscriptionSubject},
		{name: "subject zero", coordinator: coordinator, subscriber: subscriber, state: newZeroSubjectState(t), wantErr: ErrSubscriptionSubject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.coordinator.Subscribe(context.Background(), tc.subscriber, tc.state, profile)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Subscribe error=%v, want %v", err, tc.wantErr)
			}
			if got != (Subscription{}) {
				t.Fatalf("rejected Subscribe returned a subscription: %+v", got)
			}
		})
	}
	// 被拒绝的调用不得留下任何成员：既没有零值键，也没有主体 0 的分片。
	if n := len(coordinator.Profiles(0)); n != 0 {
		t.Fatalf("subject 0 has %d profiles after rejected subscribes", n)
	}
	if n := len(coordinator.Profiles(state.SubjectID())); n != 0 {
		t.Fatalf("subject %d has %d profiles after rejected subscribes", state.SubjectID(), n)
	}
}

func TestUnsubscribeRejectsInvalidSubscriberAndSubject(t *testing.T) {
	coordinator, state, subscriber := newPromiseCoordinator(t)
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
		t.Fatalf("baseline Subscribe failed: %v", err)
	}

	cases := []struct {
		name        string
		coordinator *SubscriptionCoordinator
		subscriber  SubscriberRef
		subjectID   int64
		wantErr     error
	}{
		{name: "nil coordinator", coordinator: nil, subscriber: subscriber, subjectID: state.SubjectID(), wantErr: ErrSubscriberInvalid},
		{name: "empty subscriber", coordinator: coordinator, subscriber: SubscriberRef{}, subjectID: state.SubjectID(), wantErr: ErrSubscriberInvalid},
		{name: "subject zero", coordinator: coordinator, subscriber: subscriber, subjectID: 0, wantErr: ErrSubscriptionSubject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.coordinator.Unsubscribe(context.Background(), tc.subscriber, tc.subjectID); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unsubscribe error=%v, want %v", err, tc.wantErr)
			}
		})
	}
	// 拒绝不能碰到已有的订阅。
	if got, ok := coordinator.Get(subscriber, state.SubjectID()); !ok || got.State != SubscriptionActive {
		t.Fatalf("existing subscription disturbed by rejected Unsubscribe: ok=%v state=%v", ok, got.State)
	}
}

func TestFlushSubjectRejectsMissingSubject(t *testing.T) {
	coordinator, _, _ := newPromiseCoordinator(t)
	cases := []struct {
		name        string
		coordinator *SubscriptionCoordinator
		state       *entity.SubjectSyncState
	}{
		{name: "nil coordinator", coordinator: nil, state: newZeroSubjectState(t)},
		{name: "nil state", coordinator: coordinator, state: nil},
		{name: "subject zero", coordinator: coordinator, state: newZeroSubjectState(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.coordinator.FlushSubject(context.Background(), tc.state); !errors.Is(err, ErrSubscriptionSubject) {
				t.Fatalf("FlushSubject error=%v, want %v", err, ErrSubscriptionSubject)
			}
		})
	}
}

// 信封汇为 nil 时必须返回 ErrEnvelopeSinkRequired，而不是对 nil 函数 / nil 接口
// 做调用。Subscribe / Unsubscribe / DistributeBatch 各自在前门已经检查过 sink，
// 所以 admitEnvelopes 的守卫从公开 API 不可达（前门冗余）；这里直接钉住它和
// ReliableEnvelopeSinkFunc 的 nil 接收者，二者都是导出类型 / 包内共享路径。
func TestNilEnvelopeSinksAreRejectedNotInvoked(t *testing.T) {
	envelopes := []DeliveryEnvelope{{Subscriber: SubscriberRef{Kind: SubscriberKindPlayer, ID: 1}, Kind: EnvelopeLeave}}

	var fn ReliableEnvelopeSinkFunc
	if err := fn.AdmitEnvelopes(context.Background(), envelopes); !errors.Is(err, ErrEnvelopeSinkRequired) {
		t.Fatalf("nil ReliableEnvelopeSinkFunc error=%v, want %v", err, ErrEnvelopeSinkRequired)
	}

	if err := admitEnvelopes(context.Background(), nil, envelopes); !errors.Is(err, ErrEnvelopeSinkRequired) {
		t.Fatalf("admitEnvelopes(nil sink) error=%v, want %v", err, ErrEnvelopeSinkRequired)
	}

	// 对照：非 nil 的函数汇正常被调用一次。
	calls := 0
	ok := ReliableEnvelopeSinkFunc(func(context.Context, []DeliveryEnvelope) error { calls++; return nil })
	if err := admitEnvelopes(context.Background(), ok, envelopes); err != nil || calls != 1 {
		t.Fatalf("non-nil sink: err=%v calls=%d", err, calls)
	}
}
