package selfsync_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/wiring/nest"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
)

// SelfStateClient 是 Go 客户端解码自身视图的最小写法：一个连接一个 frame.Receiver；
// 按包头的载荷类型挑出 Sync 推送，Receiver 核对基线（全量先清空、增量要接得上），
// DecodeSubjectUpdate 取出 subject 与业务 packer 写的字节，再交给业务自己的解码（生成 DAO 的 UnmarshalSync 等）。
type SelfStateClient struct {
	connection net.Conn
	receiver   *frame.Receiver
	messageID  uint32
}

// DialSelfState 连接并鉴权：先写票据，读到 ACK（ID 0、同序号、空载荷）才算登录成功。
func DialSelfState(address, ticket string, messageID uint32) (*SelfStateClient, error) {
	connection, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	if err := wire.Write(connection, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte(ticket)}}, wire.DefaultMaxPayload); err != nil {
		_ = connection.Close()
		return nil, err
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	ack, err := wire.Read(connection, wire.DefaultMaxPayload)
	if err != nil || ack.MsgID != 0 || ack.Seq != 1 || len(ack.Payload) != 0 {
		_ = connection.Close()
		return nil, fmt.Errorf("login refused: %v", err)
	}
	return &SelfStateClient{connection: connection, receiver: frame.NewReceiver(frame.Limits{}), messageID: messageID}, nil
}

// Next 读到下一份自身状态为止。其他推送（PB、Lockstep）交给各自的处理；连接上第一份全量之前收到的增量
// 被 Receiver 拒绝（frame.ErrBaselineMismatch），跳过即可，下一帧全量会到。
func (c *SelfStateClient) Next() (full bool, state string, err error) {
	for {
		_ = c.connection.SetReadDeadline(time.Now().Add(3 * time.Second))
		packet, err := wire.Read(c.connection, wire.DefaultMaxPayload)
		if err != nil {
			return false, "", err
		}
		header := wire.Header{Flags: packet.Flags}
		if header.Kind() != wire.PayloadSync || packet.MsgID != c.messageID {
			continue
		}
		applied, err := c.receiver.Receive(packet.Payload, func(decoded frame.Frame) error {
			full = decoded.Kind == frame.Full
			for _, object := range decoded.Objects {
				for _, component := range object.Components {
					update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
					if err != nil {
						return err
					}
					state = string(update.Payload.BytesCopy())
				}
			}
			return nil
		})
		switch {
		case errors.Is(err, frame.ErrBaselineMismatch):
			continue
		case err != nil:
			return false, "", err
		case applied:
			return full, state, nil
		}
	}
}

// Example_clientDecodesItsOwnState 起一个嵌入 TCP + 自身视图的服务端（同正式装配），客户端登录后先收到
// 自己的全量，金币变化后收到增量。
func Example_clientDecodesItsOwnState() {
	const kind, messageID = entity.EntityKind(62), uint32(10902)
	entity.MustRegisterEntityKindCategory(kind, entity.EntityCategoryPlayer)
	access := entity.NewManagerAccess(entity.NewEntityManager())

	// 服务端：玩家实体（packer 写 "gold=N"）、Nest Mod 自有的 Manager、嵌入 TCP。
	id, _ := entity.BuildEntityID(42, kind)
	player := entity.NewEntityBase(id, entity.EntityCategoryPlayer, false, kind)
	gold := 10
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, []byte("gold="+strconv.Itoa(gold))), nil
	}
	player.SetSyncState(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "player",
		Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}}))
	access.Manager().Add(&examplePlayer{player})
	self := selfsync.MustNew(selfsync.Config{Entities: access, Kind: kind, MessageID: messageID})
	mod := nest.NewModWithEntitySync(access, self.EntitySyncSetup())
	if err := mod.Init(viper.New()); err != nil {
		panic(err)
	}
	defer mod.EntitySync().Close(context.Background())
	config := gateway.DefaultTCPConfig()
	config.Enabled, config.Addr = true, "127.0.0.1:0"
	server, _ := gateway.NewTCPServer(config, func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, nil
	},
		gateway.AuthenticatorFunc(func(context.Context, string, net.Addr) (gateway.Principal, error) {
			return gateway.Principal{PlayerID: 42, SessionID: "example"}, nil
		}))
	runtime := gateway.NewTCPRuntime(nil, time.Second)
	_ = server.ConnectRuntime(runtime)
	_ = server.ConnectHandshake(self.Handshake())
	self.BindPlayers(runtime)
	if err := server.Start(); err != nil {
		panic(err)
	}
	defer server.Stop(context.Background())

	// 客户端。
	client, err := DialSelfState(server.Addr().String(), "ticket", messageID)
	if err != nil {
		panic(err)
	}
	defer client.connection.Close()
	for self.Online() == 0 {
		time.Sleep(time.Millisecond)
	}
	_ = mod.EntitySync().Flush(context.Background()) // 正式运行时由 Manager 的周期 tick 做
	full, state, err := client.Next()
	fmt.Println(full, state, err)

	gold = 15
	player.Sync().MarkDirty(1) // 业务 handler 里由生成 DAO 的 setter + MarkSyncDirty 完成
	_ = mod.EntitySync().Flush(context.Background())
	full, state, err = client.Next()
	fmt.Println(full, state, err)
	// Output:
	// true gold=10 <nil>
	// false gold=15 <nil>
}

type examplePlayer struct{ *entity.EntityBase }

func (p *examplePlayer) Base() *entity.EntityBase { return p.EntityBase }
