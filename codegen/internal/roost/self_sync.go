package roost

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
)

// access.player.self_sync：玩家看自己状态（Entity Sync 自身视图）的生成接线。
//
// 生成器只写可见的接线，生命周期在 core（framework/sync/entitysync/policy.SelfView 与 wiring/selfsync）：
//
//	internal/bootstrap/generated.go  PlayerSelfSync 变量；Nest 改用 kitnest.NewModWithEntitySync(EntityAccess,
//	                                 PlayerSelfSync.EntitySyncSetup())；player TCP Mod 加 .WithSelfSync(PlayerSelfSync)
//	internal/access/player/tcp/server_gen.go  WithSelfSync：Provide 时 BindPlayers，Start 时 ConnectHandshake；
//	                                 IngressMod 同名方法把 OnActive / OnClosed 接到 Game Ingress
//	protocol/def/self_sync.go        推送编号（业务文件，只在缺失时创建一次）
//
// 开关关闭时以上文件与开启前逐字节相同，不引用 wiring/selfsync。

const (
	selfSyncVar          = "PlayerSelfSync"
	selfSyncEntity       = "player"
	selfSyncProtocolPath = "protocol/def/self_sync.go"
	selfSyncDocument     = "docs/framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync"
	selfSyncImport       = "github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
)

// selfSyncEnabled 报告 access.player.self_sync 是否打开。
func selfSyncEnabled(m Manifest) bool {
	access, enabled := m.Access["player"]
	return enabled && access.SelfSync
}

// selfSyncNest 报告 service（"" 表示 shared_mods）的 Nest Mod 是否装自身视图：玩家接入所在的服务，
// 或 Nest 在 shared_mods 里时的那一个共享 Nest Mod。
func selfSyncNest(m Manifest, service string) bool {
	if !selfSyncEnabled(m) {
		return false
	}
	if service == "" {
		shared, _ := resolveMods(m.SharedMods)
		return contains(shared, "nest")
	}
	return service == m.Access["player"].Service
}

// selfSyncProtocolGroup 选推送编号所在的协议组：与玩家接入服务同名的组优先，其次 game，再其次名字最小的组。
func selfSyncProtocolGroup(m Manifest) (string, IDRange) {
	groups := m.IDs["protocol"].Groups
	for _, name := range []string{m.Access["player"].Service, "game"} {
		if span, ok := groups[name]; ok {
			return name, span
		}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0], groups[names[0]]
	}
	return "game", IDRange{Min: 10000, Max: 19999}
}

// renderSelfSyncProtocolDef 是推送编号的协议声明。编号取所在组的上限：roost add protocol 从下限往上分配，
// 两者不会很快相遇；与已有编号冲突时 roost generate 报重复，改这个文件里的 id 即可（客户端用生成的 msgid）。
func renderSelfSyncProtocolDef(m Manifest) string {
	group, span := selfSyncProtocolGroup(m)
	return fmt.Sprintf(`//go:build protocoldef

package protocoldef

// SelfSyncPush 是 access.player.self_sync 的推送编号：服务端用 PushSyncPlayer 以 Sync 载荷类型推原始
// entity-sync 帧（不经这个 PB 外壳），客户端按包头载荷类型 + msgid.MsgSelfSync 交给帧解码器
// （frame.Receiver + entitysync.DecodeSubjectUpdate，见 wiring/selfsync 的客户端示例）。
// 这个文件归项目所有，只在缺失时生成一次；编号与别的协议冲突时直接改下面的 id。
type SelfSyncPush struct {
	Payload []byte `+"`pb:\"1\"`"+`
}

//roost:protocol group=%s
type SelfSyncProtocol interface {
	//roost:msg id=%d
	SelfSync() SelfSyncPush
}
`, group, span.Max)
}

// errSelfSyncNotReady 包住 checkSelfSync 的全部拒绝，doctor 据此归类。
var errSelfSyncNotReady = errors.New("access.player.self_sync is on but the project is not ready for it")

// checkSelfSync 是 generate 与 doctor 共用的前置检查：开关打开时 Player Entity 必须声明 Entity Sync
// （sync=true 与 subjectPacker），推送编号的协议声明必须在。每条拒绝都写明要改的文件与写法。
func checkSelfSync(root string, m Manifest) error {
	if !selfSyncEnabled(m) {
		return nil
	}
	var problems []string
	entityPath := filepath.Join(root, "game", "entities", selfSyncEntity, "entity.go")
	params, err := entityMarkerParams(entityPath)
	switch {
	case os.IsNotExist(err):
		problems = append(problems, "the player Entity does not exist; run: roost add entity Player")
	case err != nil:
		problems = append(problems, err.Error())
	default:
		if params["sync"] != "true" || params["subjectPacker"] == "" {
			problems = append(problems, fmt.Sprintf(`game/entities/player/entity.go: the //roost:entity marker must declare Entity Sync; append to it: sync=true syncNamespace="player" subjectPacker=NewPlayerSyncPacker
    then write NewPlayerSyncPacker (copy the packer from %s): its view set must accept the profile Key "self" (policy.SelfProfile), e.g. entity.NamedSyncView{Profile: entity.SyncProfile{Key: "self"}, Fields: []string{"*"}}
    tag the DAO fields the client should see as dao:"persist,sync", and after each change call player.MarkSyncDirty(player.Dao().DirtyTracker().TakeSyncDirty())`, selfSyncDocument))
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(selfSyncProtocolPath))); os.IsNotExist(err) {
		problems = append(problems, selfSyncProtocolPath+" is missing; run: roost project sync (it creates the push id once)")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", errSelfSyncNotReady, strings.Join(problems, "\n  - "))
}

// entityMarkerParams 读 entity.go 里第一个 //roost:entity 标记的 key=value 参数（值可带引号）。
func entityMarkerParams(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		rest, ok := marker.Cut(strings.TrimSpace(scanner.Text()), "entity")
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		params := map[string]string{}
		for _, field := range strings.Fields(rest) {
			key, value, found := strings.Cut(field, "=")
			if !found {
				continue
			}
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			params[key] = value
		}
		return params, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: no //roost:entity marker", filepath.ToSlash(path))
}
