package lobby

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
)

// Relay 是一个 relay 实例在 etcd 里的广播，也是房间记录里“这个房间在哪个 relay 上”的那一份。
//
// etcd 契约（relay 写、lobby 读）：
//
//	键：<lobby.relay_registry_prefix><instance>        例如 /party/relays/relay-1
//	值：JSON {"instance":"relay-1","tcp_addr":"10.0.0.5:7100","ws_addr":"wss://relay1.example.com/relay","rooms":12}
//
// 约定：
//   - 键挂在 relay 进程的租约上：进程没了，键随租约过期消失，lobby 不再把新房间分给它；
//   - instance 等于键的最后一段，也是 relay 令牌里的实例名（relay 只认写着自己名字的令牌）与命令主题的最后一段，
//     只许字母数字、下划线、连字符，1～64 个字符；
//   - tcp_addr 是 host:port，ws_addr 是完整的 ws:// 或 wss:// URL（含路径）；没有开某种接入就不写那个字段；
//   - rooms 是这个实例当前的房间数，房间数变了就更新（relay 侧合并成每秒最多一次即可）。
type Relay struct {
	Instance string `json:"instance"`
	TCPAddr  string `json:"tcp_addr,omitempty"`
	WSAddr   string `json:"ws_addr,omitempty"`
	Rooms    int    `json:"rooms"`
}

// Addr 是这个实例给 transport 的地址；没有时为空。
func (r Relay) Addr(transport Transport) string {
	if transport == TransportWS {
		return r.WSAddr
	}
	return r.TCPAddr
}

// RelayDirectory 列出当前可用的 relay 实例。
type RelayDirectory interface {
	Relays(ctx context.Context) ([]Relay, error)
}

// EtcdRelays 从 etcd 前缀读 relay 广播。每次建房读一次：建房是低频操作，一次前缀读的代价远小于维护一份 watch 缓存。
type EtcdRelays struct {
	Client interface {
		GetWithPrefix(ctx context.Context, prefix string) ([]*fetcd.KV, error)
	}
	Prefix string
}

// Relays 实现 RelayDirectory。解不开或 instance 与键不符的条目跳过并记日志，不让一个坏条目挡住建房。
func (e EtcdRelays) Relays(ctx context.Context) ([]Relay, error) {
	if e.Client == nil || strings.TrimSpace(e.Prefix) == "" {
		return nil, fmt.Errorf("lobby: etcd relay directory needs a client and a prefix")
	}
	kvs, err := e.Client.GetWithPrefix(ctx, e.Prefix)
	if err != nil {
		return nil, fmt.Errorf("lobby: read relay registry %s: %w", e.Prefix, err)
	}
	relays := make([]Relay, 0, len(kvs))
	for _, kv := range kvs {
		var relay Relay
		if err := json.Unmarshal([]byte(kv.Value), &relay); err != nil || !labelPattern.MatchString(relay.Instance) ||
			relay.Instance != strings.TrimPrefix(kv.Key, e.Prefix) {
			slog.Warn("lobby: skipping unreadable relay registration", "key", kv.Key, "err", err)
			continue
		}
		relays = append(relays, relay)
	}
	return relays, nil
}

// pickRelay 选房间数最少、且有 transport 地址的实例；房间数相同时取实例名最小的（结果确定，便于排查）。
func pickRelay(relays []Relay, transport Transport) (Relay, bool) {
	var best Relay
	found := false
	for _, relay := range relays {
		if relay.Addr(transport) == "" {
			continue
		}
		if !found || relay.Rooms < best.Rooms || (relay.Rooms == best.Rooms && relay.Instance < best.Instance) {
			best, found = relay, true
		}
	}
	return best, found
}
