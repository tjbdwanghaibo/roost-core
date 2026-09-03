package ownerroute

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/bus"
)

type BusTransport[C any] struct {
	Bus         bus.IBus
	ServiceType string
	Module      string
}

func NewBusTransport[C any](b bus.IBus, serviceType string, module string) *BusTransport[C] {
	return &BusTransport[C]{Bus: b, ServiceType: serviceType, Module: module}
}

func (t *BusTransport[C]) Send(_ context.Context, sid int32, cmd *C) error {
	if t == nil || t.Bus == nil {
		return fmt.Errorf("ownerroute: bus transport is nil")
	}
	return t.Bus.SendByType(t.ServiceType, sid, t.Module, cmd)
}

func RegisterBusHandler[C any](b bus.IBus, module string, msgName string, execute func(context.Context, *C) error) error {
	if b == nil || execute == nil || module == "" || msgName == "" {
		return fmt.Errorf("ownerroute: invalid bus handler registration")
	}
	return b.Handle(module, msgName, func(ctx *bus.MsgContext) {
		var cmd C
		if err := ctx.Decode(&cmd); err != nil {
			slog.Warn("ownerroute: decode command failed", "module", module, "msg", msgName, "err", err)
			return
		}
		if err := execute(ctx.Context(), &cmd); err != nil {
			slog.Warn("ownerroute: execute command failed", "module", module, "msg", msgName, "err", err)
		}
	})
}
