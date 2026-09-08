// Package servicerpc is the client half of service-to-service RPC over the
// framework bus: typed calls, etcd-backed instance selection, a transport
// choice between the lightweight and JetStream paths, and the stable response
// status convention services answer with.
//
// It lives in the kit rather than beside any one service because it is
// infrastructure: every service needs the same client, and a business
// repository and roost-service must not each carry their own copy.
package servicerpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/bus"
	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

const defaultTimeout = 3 * time.Second

var ErrBusNil = errors.New("servicerpc: bus is nil")
var ErrResponseStatusMissing = errors.New("servicerpc: response status missing")

type BusClient struct {
	bus         bus.IBus
	serviceType string
	timeout     time.Duration
	discovery   fetcd.IDiscovery
	nextServer  atomic.Uint64
	picker      DiscoveryPicker
	transport   Transport
}

type DiscoveryPicker interface {
	Pick(ctx context.Context, serviceType string, infos []*fetcd.ServiceInfo, sequence uint64) (int32, error)
}

type Transport string

const (
	TransportLightweight Transport = "lightweight"
	TransportJetStream   Transport = "jetstream"
)

type Option func(*BusClient)

type TransportConfig interface {
	GetString(string) string
}

type ReliableBus interface {
	CallReliable(ctx context.Context, svcType string, method string, req any, resp any) error
	CallToReliable(ctx context.Context, svcType string, sid int32, method string, req any, resp any) error
}

type ResponseStatusProvider interface {
	StatusCode() int32
	StatusReason() string
}

type RoundRobinPicker struct{}

func WithTransport(transport Transport) Option {
	return func(c *BusClient) {
		if c == nil {
			return
		}
		switch transport {
		case TransportJetStream:
			c.transport = TransportJetStream
		default:
			c.transport = TransportLightweight
		}
	}
}

func OptionsFromConfig(cfg TransportConfig) []Option {
	if cfg == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(cfg.GetString("nats.rpc.transport"))) {
	case "jetstream", "js":
		return []Option{WithTransport(TransportJetStream)}
	default:
		return nil
	}
}

func NewBusClient(b bus.IBus, serviceType string, timeout time.Duration, opts ...Option) *BusClient {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	c := &BusClient{bus: b, serviceType: serviceType, timeout: timeout, picker: RoundRobinPicker{}, transport: TransportLightweight}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

func NewDiscoveredBusClient(b bus.IBus, serviceType string, timeout time.Duration, discovery fetcd.IDiscovery, opts ...Option) *BusClient {
	c := NewBusClient(b, serviceType, timeout, opts...)
	c.discovery = discovery
	return c
}

func (c *BusClient) SetDiscoveryPicker(picker DiscoveryPicker) {
	if c == nil || picker == nil {
		return
	}
	c.picker = picker
}

func (c *BusClient) Call(ctx context.Context, serverID int32, method string, req any, resp any) error {
	if c == nil || c.bus == nil {
		return ErrBusNil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if serverID != 0 {
		if c.transport == TransportJetStream {
			reliable, ok := c.bus.(ReliableBus)
			if !ok {
				return fmt.Errorf("servicerpc: reliable transport unavailable for %s", c.serviceType)
			}
			return reliable.CallToReliable(callCtx, c.serviceType, serverID, method, req, resp)
		}
		return c.bus.CallTo(callCtx, c.serviceType, serverID, method, req, resp)
	}
	if c.transport == TransportJetStream {
		reliable, ok := c.bus.(ReliableBus)
		if !ok {
			return fmt.Errorf("servicerpc: reliable transport unavailable for %s", c.serviceType)
		}
		return reliable.CallReliable(callCtx, c.serviceType, method, req, resp)
	}
	return c.bus.Call(callCtx, c.serviceType, method, req, resp)
}

func (c *BusClient) CallChecked(ctx context.Context, serverID int32, method string, req any, resp any, fallback string) error {
	if err := c.Call(ctx, serverID, method, req, resp); err != nil {
		return err
	}
	return CheckResponse(resp, fallback)
}

func CheckResponse(resp any, fallback string) error {
	code, reason, ok := ResponseStatus(resp)
	if !ok {
		return ErrResponseStatusMissing
	}
	return Check(code, reason, fallback)
}

func (c *BusClient) CallDiscoveredChecked(ctx context.Context, method string, req any, resp any, fallback string) error {
	serverID, err := c.PickServer(ctx)
	if err != nil {
		return err
	}
	return c.CallChecked(ctx, serverID, method, req, resp, fallback)
}

func (c *BusClient) PickServer(ctx context.Context) (int32, error) {
	if c == nil || c.discovery == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	infos, err := c.discovery.Discover(ctx, c.serviceType)
	if err != nil {
		return 0, fmt.Errorf("servicerpc: discover %s: %w", c.serviceType, err)
	}
	candidates := make([]*fetcd.ServiceInfo, 0, len(infos))
	for _, info := range infos {
		if info == nil || info.Sid == 0 {
			continue
		}
		candidates = append(candidates, info)
	}
	if len(candidates) == 0 {
		return 0, fmt.Errorf("servicerpc: no %s service discovered", c.serviceType)
	}
	picker := c.picker
	if picker == nil {
		picker = RoundRobinPicker{}
	}
	return picker.Pick(ctx, c.serviceType, candidates, c.nextServer.Add(1)-1)
}

func (RoundRobinPicker) Pick(_ context.Context, serviceType string, infos []*fetcd.ServiceInfo, sequence uint64) (int32, error) {
	if len(infos) == 0 {
		return 0, fmt.Errorf("servicerpc: no %s service discovered", serviceType)
	}
	idx := int(sequence % uint64(len(infos)))
	return infos[idx].Sid, nil
}

func ResponseStatus(resp any) (int32, string, bool) {
	if resp == nil {
		return 0, "", false
	}
	if provider, ok := resp.(ResponseStatusProvider); ok {
		return provider.StatusCode(), provider.StatusReason(), true
	}
	return 0, "", false
}
