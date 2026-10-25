package physicsroom

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
)

// NewOwnerClient 用创建房间前选定的 SID 构造固定客户端，后续调用不得换到其他实例。
// 服务重启后 Ref.Epoch 失效，调用者重新建房；不从旧客户端自动恢复房间。
func NewOwnerClient(b bus.IBus, ownerSID int32, timeout time.Duration) (*BusClient, error) {
	if ownerSID <= 0 {
		return nil, ErrRequestInvalid
	}
	return NewBusClient(b, ServiceType, timeout, servicerpc.WithServerID(ownerSID), servicerpc.WithTransport(servicerpc.TransportLightweight))
}
