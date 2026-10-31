package transport

import (
	"net"
	"sync/atomic"
)

// Traffic 由一次运行持有。TCP 在 socket 读写返回时计数，包含握手、迟到包和部分写入；
// 不包含内核 TCP/IP 头、重传或 TLS。未接此包装的 WS/自定义传输不能声称已测量。
type Traffic struct {
	connections atomic.Int64
	read        atomic.Int64
	written     atomic.Int64
}
type TrafficReport struct {
	MeasuredConnections int64 `json:"measured_connections"`
	ReadBytes           int64 `json:"socket_read_bytes"`
	WrittenBytes        int64 `json:"socket_written_bytes"`
}

func (t *Traffic) Snapshot() TrafficReport {
	if t == nil {
		return TrafficReport{}
	}
	return TrafficReport{MeasuredConnections: t.connections.Load(), ReadBytes: t.read.Load(), WrittenBytes: t.written.Load()}
}

type countedConn struct {
	net.Conn
	traffic *Traffic
}

func (t *Traffic) wrap(conn net.Conn) net.Conn {
	if t == nil {
		return conn
	}
	t.connections.Add(1)
	return &countedConn{Conn: conn, traffic: t}
}
func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.traffic.read.Add(int64(n))
	return n, err
}
func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.traffic.written.Add(int64(n))
	return n, err
}
