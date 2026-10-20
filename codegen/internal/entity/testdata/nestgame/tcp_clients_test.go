package nestgame

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// loadFrontend 是普通业务消息的网络入口：独立 Gate（gateFrontend）或嵌入 Game 的 TCPRuntime（embeddedFrontend）。
// 两者共用同一组模拟玩家客户端（loadClients），只差请求在服务端走哪条路，压测端行为与口径相同。
type loadFrontend interface {
	dispatch(index int, in invocation) error
	drain() error
	currentFailure() error
}

// loadClients 是模拟玩家：每玩家一条 TCP 连接、一个发请求协程（一次只有一条在途请求）与一个读协程。
// 读协程把 Sync 帧交给 onSync，其余帧当回包交给发请求协程。
type loadClients struct {
	jobs      []chan invocation
	conns     []net.Conn
	wait      sync.WaitGroup
	readWait  sync.WaitGroup
	completed atomic.Uint64
	roundTrip timings
	// stream 是长尾采集里往返时延的流名（gate / embedded）。
	stream   string
	mu       sync.Mutex
	failure  error
	stopping atomic.Bool
}

// connect 拨号并完成鉴权（票据是玩家序号 1..Players），随后为每条连接启动读、写协程。
// 所有连接都在负载开始之前建好，握手不在被测区间内。
func (clients *loadClients) connect(t *testing.T, addr string, players, maxPayload int, onSync func(index int, payload []byte) error) {
	t.Helper()
	clients.jobs = make([]chan invocation, players)
	for index := range players {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clients.conns = append(clients.conns, conn)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err = wire.Write(conn, []*wire.Packet{{Seq: 1, Payload: []byte(strconv.Itoa(index + 1))}}, 8192); err != nil {
			t.Fatal(err)
		}
		if _, err = wire.Read(conn, 8192); err != nil {
			t.Fatal(err)
		}

		replies := make(chan *wire.Packet, 1)
		readDone := make(chan struct{})
		clients.readWait.Go(func() {
			defer close(readDone)
			for {
				packet, err := wire.Read(conn, maxPayload)
				if err != nil {
					if !clients.stopping.Load() {
						clients.recordFailure(err)
					}
					return
				}
				if packet.Flags&wire.FlagSync != 0 && onSync != nil {
					if err := onSync(index, packet.Payload); err != nil {
						clients.recordFailure(err)
						return
					}
					continue
				}
				select {
				case replies <- packet:
				default:
					clients.recordFailure(gateway.ErrInvalidRequest)
					return
				}
			}
		})
		clients.jobs[index] = make(chan invocation, 8)
		jobs := clients.jobs[index]
		clients.wait.Go(func() {
			sequence := uint32(1)
			for in := range jobs {
				sequence++
				// sent 是请求离开模拟客户端的时刻，服务端分段从这里起算（见 streamResult）。
				began := time.Now()
				var payload [26]byte
				binary.LittleEndian.PutUint64(payload[:8], uint64(loadTimestamp(in.planned)))
				binary.LittleEndian.PutUint64(payload[8:16], uint64(loadTimestamp(in.admitted)))
				binary.LittleEndian.PutUint64(payload[16:24], uint64(loadTimestamp(began)))
				if in.change {
					payload[24] = 1
				}
				if in.sample {
					payload[25] = 1
				}
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				err := wire.Write(conn, []*wire.Packet{{MsgID: 1, Seq: sequence, Payload: payload[:]}}, maxPayload)
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
					clients.recordFailure(err)
					continue
				}
				roundTrip := time.Since(began)
				clients.roundTrip.add(roundTrip, in.sample)
				if c := tail.Load(); c != nil {
					c.observe(clients.stream, roundTrip)
				}
				clients.completed.Add(1)
			}
		})
	}
}

// decodeLoadRequest 是服务端对模拟客户端请求的解码（独立 Gate 的 Game Ingress 与嵌入 TCP 共用）。
func decodeLoadRequest(id uint32, kind wire.PayloadKind, payload []byte) (invocation, error) {
	if id != 1 || kind != wire.PayloadProtobuf || len(payload) != 26 {
		return invocation{}, gateway.ErrInvalidRequest
	}
	return invocation{planned: loadTime(int64(binary.LittleEndian.Uint64(payload))), admitted: loadTime(int64(binary.LittleEndian.Uint64(payload[8:]))),
		sent: loadTime(int64(binary.LittleEndian.Uint64(payload[16:]))), change: payload[24] == 1, sample: payload[25] == 1}, nil
}

func (clients *loadClients) dispatch(index int, in invocation) error {
	select {
	case clients.jobs[index] <- in:
		return nil
	default:
		return gateway.ErrAdmissionFull
	}
}

func (clients *loadClients) drain() error {
	for _, jobs := range clients.jobs {
		close(jobs)
	}
	clients.wait.Wait()
	return clients.currentFailure()
}

// close 在服务端停止之后关闭客户端连接并等读协程退出；之后的读错误不算失败。
func (clients *loadClients) close() {
	clients.stopping.Store(true)
	for _, conn := range clients.conns {
		_ = conn.Close()
	}
	clients.readWait.Wait()
}

func (clients *loadClients) recordFailure(err error) {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	if clients.failure == nil {
		clients.failure = err
	}
}

func (clients *loadClients) currentFailure() error {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	return clients.failure
}
