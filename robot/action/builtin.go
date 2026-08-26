package action

import (
	"context"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/cube-core/robot"
	"github.com/tjbdwanghaibo/cube-core/robot/session"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"
)

// Built-in framework actions. Business actions register on top of these;
// the gateway handshake and application login stay business territory (a
// RegisterCall usage plus the Config.Auth hook), but connect and the wait
// primitives are protocol-agnostic.
const (
	NameConnect  = "connect"
	NameWait     = "wait"
	NameWaitPush = "wait_push"
)

func registerBuiltins(reg *Registry) {
	reg.MustRegister(Func{ActionName: NameConnect, Handle: runConnect})
	reg.MustRegister(Func{ActionName: NameWait, Handle: runWait})
	reg.MustRegister(Func{ActionName: NameWaitPush, Handle: runWaitPush})
}

// runConnect dials the configured transport, installs the session, and —
// when the robot has an AuthProvider — fires the gateway handshake packet.
// Params (all optional): endpoint, type, dial_timeout.
func runConnect(ctx context.Context, rb *robot.Context, param any) error {
	cfg := rb.Transport
	params := ParamsOf(rb, param)
	if endpoint := params.String("endpoint", ""); endpoint != "" {
		cfg.Endpoint = endpoint
	}
	if transportType := params.String("type", ""); transportType != "" {
		cfg.Type = transportType
	}
	if timeout := params.Duration("dial_timeout", 0); timeout > 0 {
		cfg.DialTimeout = timeout
	}
	conn, err := transport.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	rb.SetSession(session.New(conn, rb.Protocols))
	if rb.Auth != nil {
		packet, err := rb.Auth(rb)
		if err != nil {
			return fmt.Errorf("auth packet: %w", err)
		}
		if packet != nil {
			if err := rb.Session().SendPacket(packet); err != nil {
				return fmt.Errorf("auth send: %w", err)
			}
		}
	}
	return nil
}

// runWait sleeps for the "duration" param (default 1s), honoring ctx.
func runWait(ctx context.Context, rb *robot.Context, param any) error {
	d := ParamsOf(rb, param).Duration("duration", time.Second)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runWaitPush blocks until one push with msg id "msg" arrives (param
// "timeout" bounds the wait, default 15s).
func runWaitPush(ctx context.Context, rb *robot.Context, param any) error {
	params := ParamsOf(rb, param)
	msgID := params.Uint32("msg", 0)
	if msgID == 0 {
		return fmt.Errorf("wait_push: msg param is required")
	}
	timeout := params.Duration("timeout", 15*time.Second)
	s := rb.Session()
	if s == nil {
		return session.ErrClosed
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := s.WaitPush(waitCtx, msgID, nil)
	return err
}
