package split

import (
	"context"
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/bus"
)

// stubBus satisfies the bus capability so a caller process can be wired.
//
// It lives in its own file so the four files a reader actually reads —
// doc, consumer, and the two wirings — stay about the wiring. Every method
// fails rather than succeeding quietly: a test that started depending on the
// bus should have to say so, not pass against a no-op.
//
// A real bus cannot be constructed without NATS, which is why this is a stub
// where the Redis client is the real type.
type stubBus struct{}

func (stubBus) Send(int32, string, any) error { return errors.New("stubBus: Send") }

func (stubBus) SendByType(string, int32, string, any) error {
	return errors.New("stubBus: SendByType")
}

func (stubBus) Broadcast(string, string, any) error { return errors.New("stubBus: Broadcast") }

func (stubBus) BroadcastAll(string, any) error { return errors.New("stubBus: BroadcastAll") }

func (stubBus) Call(context.Context, string, string, any, any) error {
	return errors.New("stubBus: Call")
}

func (stubBus) CallTo(context.Context, string, int32, string, any, any) error {
	return errors.New("stubBus: CallTo")
}

func (stubBus) CallWithTimeout(string, string, any, any, time.Duration) error {
	return errors.New("stubBus: CallWithTimeout")
}

func (stubBus) CallAsync(string, string, any, func([]byte, error)) {}

func (stubBus) Handle(string, string, bus.HandlerFunc) error {
	return errors.New("stubBus: Handle")
}

func (stubBus) HandleRpc(string, bus.RpcHandlerFunc) error { return nil }

var _ bus.IBus = stubBus{}
