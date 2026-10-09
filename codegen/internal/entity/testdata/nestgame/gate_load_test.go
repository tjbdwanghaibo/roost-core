package nestgame

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

// 可选正式接入负载：普通业务改走 TCP → Core NATS → Game → Nest，HB 保持原 ticker。
// Entity/DAO 由同一生成模块提供；SYNC=1 叠加正式 Interest/Sync。Saga/Remote 仍是独立专项。
type gateFrontend struct {
	jobs      []chan invocation
	clients   []net.Conn
	wait      sync.WaitGroup
	server    *gateway.TCPServer
	gate      *gateway.Gate
	ingress   *gateway.GameIngress
	nats      []*driver.Client
	completed atomic.Uint64
	mu        sync.Mutex
	failure   error
	roundTrip timings
	readWait  sync.WaitGroup
	stopping  atomic.Bool
}

func newGateFrontend(t *testing.T, engine *nest.NestMgr, name nest.HandlerName, units []*Unit, c gameConfig, stateSync *syncGameLoad) *gateFrontend {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Fatal("Gate load requires private ROOST_DATAENGINE_IT_NATS_URL")
	}
	f := &gateFrontend{jobs: make([]chan invocation, c.Players)}
	config := gateway.DefaultConfig()
	config.Namespace = fmt.Sprintf("roost.gate.load.n%d", time.Now().UnixNano())
	gateID, gameID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}, gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	raw := func(role string, id gateway.ProcessIdentity) fnats.RawClient {
		client, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		f.nats = append(f.nats, client)
		prefix, _ := gateway.InboxPrefix(config.Namespace, role, id)
		scoped, err := client.ForInbox(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return scoped
	}
	gameRaw, gateRaw := raw("game", gameID), raw("gate", gateID)
	auth := gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		id, err := strconv.Atoi(ticket)
		if err != nil || id < 1 || id > c.Players {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: int64(id), SessionID: ticket, ServerID: 22}, nil
	})
	matches := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return (role == "game" && id == gameID) || (role == "gate" && id == gateID), nil
	}
	var err error
	deps := gateway.GameDependencies{Client: gameRaw, Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, Dispatch: func(ctx context.Context, session gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if id != 1 || kind != wire.PayloadProtobuf || len(payload) != 18 {
			return nil, gateway.ErrInvalidRequest
		}
		index := int(session.Principal().PlayerID) - 1
		in := invocation{planned: loadTime(int64(binary.LittleEndian.Uint64(payload))), admitted: loadTime(int64(binary.LittleEndian.Uint64(payload[8:]))), change: payload[16] == 1, sample: payload[17] == 1}
		_, err := engine.Request(ctx, name, units[index].ID(), nest.NewParams(in))
		if err != nil {
			return nil, err
		}
		return &gateway.TCPResponse{MessageID: 2, Sequence: seq}, nil
	}}
	if stateSync != nil {
		deps.OnActive = stateSync.active
		deps.OnClosed = stateSync.closed
	}
	f.ingress, err = gateway.NewGameIngress(config, gameID, deps)
	if stateSync != nil {
		stateSync.transport.Game = f.ingress
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ingress.Start(); err != nil {
		t.Fatal(err)
	}
	f.gate, err = gateway.NewGate(config, gateID, gateway.GateDependencies{Client: gateRaw, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := gateway.DefaultTCPConfig()
	tcp.Addr = "127.0.0.1:0"
	tcp.MaxConnectionsPerIP = c.Players
	tcp.MaxPayloadBytes = uint32(config.MaxPayloadBytes)
	tcp.RequestRate = 0
	f.server, err = gateway.NewTCPServer(tcp, f.gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.ConnectHandshake(f.gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, client := range []fnats.RawClient{gameRaw, gateRaw} {
		if err = client.FlushContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.stopping.Store(true)
		_ = f.gate.Stop(ctx)
		_ = f.server.Stop(ctx)
		_ = f.ingress.Stop(ctx)
		for _, conn := range f.clients {
			_ = conn.Close()
		}
		f.readWait.Wait()
		for _, client := range f.nats {
			client.Close()
		}
	})
	for index := range c.Players {
		conn, err := net.DialTimeout("tcp", f.server.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		f.clients = append(f.clients, conn)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err = wire.Write(conn, []*wire.Packet{{Seq: 1, Payload: []byte(strconv.Itoa(index + 1))}}, 8192); err != nil {
			t.Fatal(err)
		}
		if _, err = wire.Read(conn, 8192); err != nil {
			t.Fatal(err)
		}

		replies := make(chan *wire.Packet, 1)
		readDone := make(chan struct{})
		f.readWait.Go(func() {
			defer close(readDone)
			for {
				packet, err := wire.Read(conn, config.MaxPayloadBytes)
				if err != nil {
					if !f.stopping.Load() {
						f.recordFailure(err)
					}
					return
				}
				if packet.Flags&wire.FlagSync != 0 && stateSync != nil {
					if err := stateSync.apply(index, packet.Payload); err != nil {
						f.recordFailure(err)
						return
					}
					continue
				}
				select {
				case replies <- packet:
				default:
					f.recordFailure(gateway.ErrInvalidRequest)
					return
				}
			}
		})
		f.jobs[index] = make(chan invocation, 8)
		jobs := f.jobs[index]
		f.wait.Go(func() {
			sequence := uint32(1)
			for in := range jobs {
				sequence++
				var payload [18]byte
				binary.LittleEndian.PutUint64(payload[:8], uint64(loadTimestamp(in.planned)))
				binary.LittleEndian.PutUint64(payload[8:16], uint64(loadTimestamp(in.admitted)))
				if in.change {
					payload[16] = 1
				}
				if in.sample {
					payload[17] = 1
				}
				began := time.Now()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				err := wire.Write(conn, []*wire.Packet{{MsgID: 1, Seq: sequence, Payload: payload[:]}}, config.MaxPayloadBytes)
				if err == nil {
					var reply *wire.Packet
					select {
					case reply = <-replies:
					case <-readDone:
						err = gateway.ErrSessionClosed
					case <-time.After(5 * time.Second):
						err = context.DeadlineExceeded
					}
					if err == nil && (reply == nil || reply.MsgID != 2 || reply.Seq != sequence) {
						err = gateway.ErrInvalidRequest
					}
				}
				if err != nil {
					f.mu.Lock()
					if f.failure == nil {
						f.failure = err
					}
					f.mu.Unlock()
					continue
				}
				f.roundTrip.add(time.Since(began), in.sample)
				f.completed.Add(1)
			}
		})
	}
	return f
}
func (f *gateFrontend) dispatch(index int, in invocation) error {
	select {
	case f.jobs[index] <- in:
		return nil
	default:
		return gateway.ErrAdmissionFull
	}
}
func (f *gateFrontend) drain() error {
	for _, jobs := range f.jobs {
		close(jobs)
	}
	f.wait.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failure
}

func (f *gateFrontend) recordFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure == nil {
		f.failure = err
	}
}
