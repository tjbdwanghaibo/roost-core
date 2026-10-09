package gate

import (
	"context"
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

const IncarnationMetadata = "access.incarnation"
const IngressReadyMetadata = "access.ingress_ready"

// FixedGameResolver 从已有发现结果选固定 SID。多个不同代次不猜测接管，
// 返回的候选仍由 gateway 的 singleton checker 核对权威身份。
func FixedGameResolver(discovery etcd.IDiscovery, serviceType string) func(context.Context, int32) (gateway.ProcessIdentity, error) {
	return func(ctx context.Context, sid int32) (gateway.ProcessIdentity, error) {
		if discovery == nil || serviceType == "" || sid <= 0 {
			return gateway.ProcessIdentity{}, gateway.ErrInvalidRequest
		}
		infos, err := discovery.Discover(ctx, serviceType)
		if err != nil {
			return gateway.ProcessIdentity{}, err
		}
		var candidate gateway.ProcessIdentity
		for _, info := range infos {
			if info == nil || info.Sid != sid || info.Metadata[IngressReadyMetadata] != "true" {
				continue
			}
			id := gateway.ProcessIdentity{ServerID: sid, Incarnation: info.Metadata[IncarnationMetadata]}
			if id.Validate() != nil {
				continue
			}
			if candidate.ServerID != 0 && candidate != id {
				return gateway.ProcessIdentity{}, errors.New("gate wiring: conflicting game incarnations")
			}
			candidate = id
		}
		if candidate.ServerID == 0 {
			return candidate, gateway.ErrTransportUnavailable
		}
		return candidate, nil
	}
}

// GateCandidates 用于广播的候选快照；广播结果逐 Gate 区分拒绝和未知。
func GateCandidates(discovery etcd.IDiscovery, serviceType string) func(context.Context) ([]gateway.ProcessIdentity, error) {
	return func(ctx context.Context) ([]gateway.ProcessIdentity, error) {
		if discovery == nil || serviceType == "" {
			return nil, gateway.ErrInvalidRequest
		}
		infos, err := discovery.Discover(ctx, serviceType)
		if err != nil {
			return nil, err
		}
		var result []gateway.ProcessIdentity
		for _, info := range infos {
			if info == nil || info.Metadata[IngressReadyMetadata] != "true" {
				continue
			}
			id := gateway.ProcessIdentity{ServerID: info.Sid, Incarnation: info.Metadata[IncarnationMetadata]}
			if id.Validate() == nil {
				result = append(result, id)
			}
		}
		return result, nil
	}
}

// register 在监听和角色订阅已建立后发布候选，不取代每次 Bind 的 singleton/Ready 检查。
// 发现登记是接线生命周期：注册循环仍由原 IDiscovery 唯一实现。
func (deps *connections) register(role, address string, timeout time.Duration) error {
	if deps.discovery == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := deps.client.FlushContext(ctx); err != nil {
		return err
	}
	info := &etcd.ServiceInfo{ServiceType: role, Sid: deps.identity.ServerID, Addr: address, Metadata: map[string]string{IncarnationMetadata: deps.identity.Incarnation, IngressReadyMetadata: "true"}}
	if err := deps.discovery.Register(ctx, info); err != nil {
		return err
	}
	deps.registered = true
	return nil
}
func (deps *connections) deregister(ctx context.Context) error {
	if !deps.registered {
		return nil
	}
	if err := deps.discovery.Deregister(ctx); err != nil {
		return err
	}
	deps.registered = false
	return nil
}
