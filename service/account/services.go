package account

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// 多个服务（方案 2026-11-16 §4.1.2）：一套 account 同时服务工程里的几个服务——传统区服的 game、开房间的
// lobby 与 relay。account.services 列出它们与各自的类型：
//
//	lobby  无区服；客户端凭游戏账号 token（game_token.go）进入。
//	relay  无区服；客户端凭 lobby 签的房间令牌进入，不经 account。
//	game   传统区服：区服记录（GameServer）归属到一个 kind=game 的服务（GameServer.Service）。
//
// 区服号是全局的一个号段：所有 game 服务的区服共用 Servers 存储与区服索引（servers.go），服号不能重复。
//
// 没有写 account.services 的工程按“单一 game 服务”处理，行为与以前完全相同：区服记录不带服务名，
// ListServices 答空。写了的工程，区服记录没写服务名时归到唯一的那个 game 服务（serviceOf）；
// game 服务不止一个时，登记区服必须写明服务名。

// ServiceKind 是 account.services 里一个服务的类型。
type ServiceKind string

const (
	ServiceKindGame  ServiceKind = "game"
	ServiceKindLobby ServiceKind = "lobby"
	ServiceKindRelay ServiceKind = "relay"
)

// ServiceSpec 是 account.services 里的一个服务。
type ServiceSpec struct {
	// Name 是工程里的服务名（roost.yaml 的服务名），写法同游戏名（gameNamePattern）。
	Name string      `json:"name"`
	Kind ServiceKind `json:"kind"`
}

// newServiceTable 检查 account.services，按服务名升序返回，并给出“没写服务名的区服”归属的那个 game 服务
// （game 服务恰好一个时是它，否则为空）。名字不合写法、重复、类型不认识，一律报错。
func newServiceTable(services []ServiceSpec) ([]ServiceSpec, string, error) {
	table := make([]ServiceSpec, 0, len(services))
	var games []string
	for _, service := range services {
		if !gameNamePattern.MatchString(service.Name) {
			return nil, "", fmt.Errorf("account: service name %q must be lowercase letters, digits, '_' or '-', start with a letter and be at most %d bytes", service.Name, MaxGameNameBytes)
		}
		switch service.Kind {
		case ServiceKindGame:
			games = append(games, service.Name)
		case ServiceKindLobby, ServiceKindRelay:
		default:
			return nil, "", fmt.Errorf("account: service %q has kind %q; want game, lobby or relay", service.Name, service.Kind)
		}
		if slices.ContainsFunc(table, func(listed ServiceSpec) bool { return listed.Name == service.Name }) {
			return nil, "", fmt.Errorf("account: service %q is listed twice", service.Name)
		}
		table = append(table, service)
	}
	slices.SortFunc(table, func(a, b ServiceSpec) int { return strings.Compare(a.Name, b.Name) })
	defaultGame := ""
	if len(games) == 1 {
		defaultGame = games[0]
	}
	return table, defaultGame, nil
}

// CheckGamesAndServices 检查 account.games 与 account.services 的写法（名字、重复、寿命、类型），与 New 的检查
// 是同一处。wiring 在读配置时调它，写错的配置在进程启动的配置阶段就报出来。
func CheckGamesAndServices(games []GameSpec, services []ServiceSpec) error {
	if _, err := newGameTable(games); err != nil {
		return err
	}
	_, _, err := newServiceTable(services)
	return err
}

// ListServices 返回 account.services，按服务名升序；没有配置时答空（单一 game 服务的工程）。只读、不需要调用方身份。
func (s *Service) ListServices(context.Context) ([]ServiceSpec, error) {
	if len(s.services) == 0 {
		return nil, nil
	}
	return slices.Clone(s.services), nil
}

// serviceOf 是区服记录归属的服务：记录写了就是它，没写时是唯一的 game 服务（没有配置 account.services、
// 或者 game 服务不止一个时为空）。ListServers / GetServer 交出去的记录经它填好。
func (s *Service) serviceOf(server GameServer) string {
	if server.Service != "" {
		return server.Service
	}
	return s.defaultGameService
}

// checkServerService 决定 UpsertServer 写进记录的服务名：调用方没写时沿用记录里已有的，再没有就归到唯一的
// game 服务。没有配置 account.services 时不能写服务名；配置了时服务名必须是一个 kind=game 的服务。
func (s *Service) checkServerService(requested, current string) (string, error) {
	service := requested
	if service == "" {
		service = current
	}
	if len(s.services) == 0 {
		if service != "" {
			return "", fmt.Errorf("%w: server names service %q but account.services is not configured", ErrServerInvalid, service)
		}
		return "", nil
	}
	if service == "" {
		service = s.defaultGameService
	}
	if service == "" {
		return "", fmt.Errorf("%w: name the server's service (account.services does not list exactly one game service)", ErrServerInvalid)
	}
	at := slices.IndexFunc(s.services, func(listed ServiceSpec) bool { return listed.Name == service })
	if at < 0 || s.services[at].Kind != ServiceKindGame {
		return "", fmt.Errorf("%w: service %q is not a game service in account.services", ErrServerInvalid, service)
	}
	return service, nil
}
