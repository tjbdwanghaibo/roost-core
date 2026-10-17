package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
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
//	game/entities/player/player_sync_packer_gen.go  Player 标记没写 subjectPacker= 时的默认 packer（entity 生成器
//	                                 带 -self-sync Player 写出；"self" 视图含 DAO 全部 sync 字段）
//
// 开关关闭时以上文件与开启前逐字节相同，不引用 wiring/selfsync。

const (
	selfSyncVar          = "PlayerSelfSync"
	selfSyncEntity       = "player"
	selfSyncEntityName   = "Player"
	selfSyncProtocolPath = "protocol/def/self_sync.go"
	selfSyncDocument     = "docs/framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync"
	// selfSyncDocumentURL 是给生成工程里的人看的地址：core 文档不在生成工程里，相对路径打不开。
	selfSyncDocumentURL = "https://github.com/tjbdwanghaibo/roost-core/blob/main/" + selfSyncDocument
	selfSyncImport      = "github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
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

// renderSelfSyncGuide 是 self_sync 打开时接在生成工程 docs/PLAYER_ACCESS_TCP.zh-CN.md 末尾的一节；
// 开关关闭时不出现，文档与开启前逐字节相同。
func renderSelfSyncGuide() string {
	return `
## 8. 玩家看自己的状态（self_sync）

roost.yaml 的 access.player.self_sync: true 已打开：每个在线玩家经 Entity Sync 收到自己的 Player
（视图 self），登录后第一帧是全量，之后每次 handler 提交的变化是增量，断线重连从全量开始。

要做的只有一件事：给客户端要看的 DAO 字段写 dao:"persist,sync"，只经生成的 setter 在 Nest handler
里修改（不需要手动标脏）。Player 的 //roost:entity 标记不用改：开关打开时按 sync=true 生成，没写
subjectPacker= 时生成器写 game/entities/player/player_sync_packer_gen.go（self 视图包含 DAO 全部
sync 字段，roost generate / project sync 会重写）。代价：标了 sync 的字段默认全部对玩家本人可见。

需要裁剪字段或别的视图（AOI 里别人看到的视图、按字段权限）时自己写 packer，视图集要认识
policy.SelfProfile，然后在标记上写 subjectPacker=<你的工厂>；生成的那份随之删除。去掉
subjectPacker= 后它会回来。DAO 没有 sync 字段时 roost project doctor 给警告：客户端收不到任何东西。

运行时规则（跟不上就断开、监控指标、客户端解码）见 ` + selfSyncDocumentURL + `。
`
}

// errSelfSyncNotReady 包住 checkSelfSync 的全部拒绝，doctor 据此归类。
var errSelfSyncNotReady = errors.New("access.player.self_sync is on but the project is not ready for it")

// selfSyncStatus 是 checkSelfSync 通过时的结论：Player 用哪个 packer，以及给 doctor 的警告（没有就空）。
type selfSyncStatus struct {
	packer  string
	warning string
}

// checkSelfSync 是 generate 与 doctor 共用的前置检查。开关打开时：Player Entity 必须存在；标记缺省按
// sync=true，没写 subjectPacker= 时生成器写默认 packer（game/entities/player/player_sync_packer_gen.go），
// 这要求 Player 恰好一个 DAO；推送编号的协议声明必须在。每条拒绝都写明要改的文件与写法。
// 默认 packer 的 DAO 没有标 sync 的字段时只给警告：工程照常生成、编译，客户端收不到任何字段。
func checkSelfSync(root string, m Manifest) (selfSyncStatus, error) {
	if !selfSyncEnabled(m) {
		return selfSyncStatus{}, nil
	}
	var problems []string
	var status selfSyncStatus
	entityDir := filepath.Join(root, "game", "entities", selfSyncEntity)
	if _, err := os.Stat(filepath.Join(entityDir, "entity.go")); os.IsNotExist(err) {
		problems = append(problems, "the player Entity does not exist; run: roost add entity "+selfSyncEntityName)
	} else if plan, err := entity.InspectSelfSync(entityDir, selfSyncEntityName); err != nil {
		problems = append(problems, fmt.Sprintf("%v\n    (see %s)", err, selfSyncDocumentURL))
	} else {
		status.packer = plan.Packer
		if plan.DefaultPacker {
			status.packer += " (generated in game/entities/" + selfSyncEntity + "/" + plan.PackerFile + ")"
		}
		if plan.NoSyncFields {
			status.warning = plan.NoSyncFieldsWarning(selfSyncEntityName)
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(selfSyncProtocolPath))); os.IsNotExist(err) {
		problems = append(problems, selfSyncProtocolPath+" is missing; run: roost project sync (it creates the push id once)")
	}
	if len(problems) == 0 {
		return status, nil
	}
	return selfSyncStatus{}, fmt.Errorf("%w:\n  - %s", errSelfSyncNotReady, strings.Join(problems, "\n  - "))
}
