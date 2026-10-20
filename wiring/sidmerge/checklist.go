package sidmerge

import (
	"fmt"
	"strconv"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
)

// 合服清单：跟着 sid 走、合服时要处理的东西。一部分由本包检查或迁移（数据库、outbox、saga、WAL），一部分只能
// 列出来由运维或业务处理（持久总线消息、需要业务语义的合并）。清单只读配置、不连任何依赖，doctor 的
// merge-readiness 与 cmd/sidmerge checklist 打印同一份。

// BusItem 是一种会把消息留在持久流 / 存储里的总线用法。每一项都按源码核对过（Source），写明发往一个 sid 的消息
// 落在哪里、由谁消费、A 退役后会怎样、运维要做什么。
type BusItem struct {
	Name string `json:"name"`
	// Persistent 为 false 表示这份配置下它不落盘（例如 RPC 走 NATS request-reply），列出只为说明不用处理。
	Persistent bool `json:"persistent"`
	// Where 是流名 / 存储位置与消费者名的形状（<sid> 换成要退役的服）。
	Where string `json:"where,omitempty"`
	// AddressedToSID 表示消息按 sid 寻址（只有那个 sid 的进程消费）；false 表示共享消费者，任何一个 game 进程都会消费。
	AddressedToSID bool   `json:"addressed_to_sid"`
	AfterMerge     string `json:"after_merge"`
	Action         string `json:"action"`
	Source         string `json:"source"`
}

// busChecklist 是按 A 的配置列出的持久总线清单（docs/framework/PLAYER-MIGRATION.md §6 的“持久总线消息清单”）。
func busChecklist(from side) []BusItem {
	sid := strconv.FormatInt(int64(from.sid), 10)
	items := []BusItem{
		{
			Name: "dataengine effects stream", Persistent: true,
			Where:          fmt.Sprintf("JetStream stream %s; durable consumers are named by the business (game-demo: game-level-up-mail, game-activity-phase) and by saga (roost-saga-start, roost-saga-start-result), shared by every game process", from.effects),
			AddressedToSID: false,
			AfterMerge:     "effects already published are consumed by B (or any game process) through the same durables; effects A staged but never published stay in A's outbox",
			Action:         "none beyond the outbox check (outbox:<sid>)",
			Source:         "wiring/dataengine SubscribeEffects; framework/dataengine/engine/runtime.go (outbox drains at the next start)",
		},
		{
			Name: "saga stream", Persistent: true,
			Where:          fmt.Sprintf("JetStream stream %s (subjects %s.>); step consumers are shared durables (game-demo: game-gift-<step>)", from.saga.Stream, from.saga.SubjectPrefix),
			AddressedToSID: false,
			AfterMerge:     "a step command can carry the sender's bound sid in its payload (game-demo gift.State.FromSID) and is admitted only by that sid's process, which no longer exists",
			Action:         "every saga must be terminal before the merge (checked: saga:<database>)",
			Source:         "demo/internal/service/game/gift_saga.go.tmpl (admitOwned, giftHandoffRouter)",
		},
	}
	syncbus := BusItem{
		Name: "syncbus", AddressedToSID: true,
		Source: "framework/sync/syncbus/driver/jetstream.go (durableSyncName)",
	}
	if from.syncbus.Transport == "jetstream" || from.syncbus.Transport == "js" {
		syncbus.Persistent = true
		syncbus.Where = fmt.Sprintf("JetStream stream %s; one durable per topic per sid: sync_<topic>_%s_<hash> (and <topic>.live)", from.syncbus.Stream, sid)
		syncbus.AfterMerge = "A's durables keep receiving broadcast copies nobody consumes until the stream's max age drops them; B has its own durables, nothing addressed only to A is lost"
		syncbus.Action = fmt.Sprintf("after the merge delete A's durables: nats consumer ls %s | grep _%s_ ; nats consumer rm %s <name>", from.syncbus.Stream, sid, from.syncbus.Stream)
	} else {
		syncbus.Where = "core NATS (syncbus.transport: " + from.syncbus.Transport + ")"
		syncbus.AfterMerge = "not persisted"
		syncbus.Action = "none"
	}
	items = append(items, syncbus)
	rpc := BusItem{
		Name: "service rpc", AddressedToSID: true,
		Source: "infra/network/bus/jetstream_rpc.go (subscribeJetStreamRPCMethod)",
	}
	if from.bus.JetStreamRPC {
		rpc.Persistent = true
		rpc.Where = fmt.Sprintf("JetStream stream %s; per-instance durables rpc_<service>_%s_<method> for requests sent to %s.svc.<service>.%s (game-demo: the gift step handoff)",
			from.bus.RPC.RequestStream, sid, from.bus.Prefix, sid)
		rpc.AfterMerge = fmt.Sprintf("requests addressed to sid %s wait until their request TTL (%s) and the callers time out; nothing replays them to B", sid, from.bus.RPC.RequestTTL)
		rpc.Action = fmt.Sprintf("before the merge confirm A's instance durables have no pending requests (nats consumer info %s rpc_..._%s_...), then delete them", from.bus.RPC.RequestStream, sid)
	} else {
		rpc.Where = "NATS request-reply (nats.rpc.transport: core)"
		rpc.AfterMerge = "not persisted: a request to a retired sid fails at once with no responders"
		rpc.Action = "none"
	}
	items = append(items, rpc)
	reliable := BusItem{
		Name: "reliable bus", AddressedToSID: true,
		Source: "infra/network/bus/reliable.go (inbox / DLQ keyed by consumer <service>:<sid>)",
	}
	if from.bus.ReliableEnabled {
		reliable.Persistent = true
		reliable.Where = fmt.Sprintf("Redis under nats.reliable.prefix (%q, empty is roost:bus): dedupe inbox and dead-letter list of consumer game:%s", from.bus.ReliablePrefix, sid)
		reliable.AfterMerge = "A's dead letters are no longer replayed by anyone"
		reliable.Action = "before stopping A, list its dead letters (admin bus.dlq.*) and replay them to B or drop them; this is a business decision"
	} else {
		reliable.Where = "disabled (nats.reliable.enabled: false)"
		reliable.AfterMerge = "not persisted"
		reliable.Action = "none"
	}
	items = append(items, reliable)
	items = append(items, BusItem{
		Name: "remote entity snapshot interests", Persistent: true, AddressedToSID: true,
		Where:      "Redis interest table, consumer sid " + sid,
		AfterMerge: "A's interests expire after remote_entity.snapshot_interest_ttl; nothing to do",
		Action:     "none",
		Source:     "framework/remoteentity/snapshot_client.go",
	})
	return items
}

// BusinessItem 是需要业务语义的合并（设计 §6.2）：框架只列出，不实现。
type BusinessItem struct {
	Name     string `json:"name"`
	Today    string `json:"today"`
	Decision string `json:"decision"`
}

var businessChecklist = []BusinessItem{
	{"World singleton", "UniqueID = sid; the World DAO is in a shared database, so after the merge only World(B) loads; World(A)'s document stays where it is", "fold counters / progress into World(B), or drop them"},
	{"per-server leaderboards", "board names often contain the sid", "keep A's board read-only beside B's, or recompute a merged board"},
	{"activity groups (activity.groups_file)", "groups list game sids; a member whose process is not live holds the activity back", "remove A from its group; carry A's contribution over if needed"},
	{"cross-server global routing", "service/global RouteBinding binds game sids to global instances", "unbind A"},
	{"server-scoped unique names", "no conflict when the name directory is global", "a project with per-server unique names needs a rename rule"},
	{"mail, chat, pending purchases", "per-player data follows the PlayerID and is unaffected", "per-server channels / broadcasts are the business's to merge"},
	{"sids stored in business data", "e.g. a saga state's sender sid, a document that records its origin server", "terminal sagas are required (checked); any other stored sid is the business's to rewrite or keep"},
}

// DatabaseItem 是一个逻辑库在合服里的处理方式。
type DatabaseItem struct {
	Logical string `json:"logical"`
	// ServerScoped 为 true 的逻辑库有 dbscope=sid 的 DAO：<物理名>_<A> 由 sidmerge migrate 搬进 <物理名>_<B>。
	ServerScoped bool `json:"server_scoped"`
	// Physical 是这份配置里的物理名（按 sid 分库时不含 _<sid>）。
	Physical string `json:"physical"`
	Handling string `json:"handling"`
}

// Checklist 是一个工程的合服清单。
type Checklist struct {
	SID       int32          `json:"sid"`
	Databases []DatabaseItem `json:"databases"`
	Control   string         `json:"control_database"`
	Saga      string         `json:"saga_database"`
	Bus       []BusItem      `json:"bus"`
	Business  []BusinessItem `json:"business"`
}

// BuildChecklist 按一份服务配置给出合服清单，不连任何依赖。declared 为 nil 时取本进程登记的逻辑库。
func BuildChecklist(server Server, declared []coredata.LogicalDatabase) (Checklist, error) {
	if declared == nil {
		declared = coredata.DeclaredDatabases()
	}
	if server.SID == 0 && server.Config != nil {
		var identity app.ServiceIdentity
		if err := app.LoadConfig(server.Config, &identity); err != nil {
			return Checklist{}, fmt.Errorf("sidmerge: %w", err)
		}
		server.SID = identity.Sid
	}
	resolved, err := resolveSide(server)
	if err != nil {
		return Checklist{}, err
	}
	list := Checklist{SID: resolved.sid, Control: resolved.controlDB, Saga: resolved.saga.Store.Database,
		Bus: busChecklist(resolved), Business: businessChecklist}
	for _, database := range declared {
		item := DatabaseItem{Logical: database.Name, ServerScoped: database.ServerScoped, Physical: resolved.databases.Physical(database.Name)}
		if database.ServerScoped {
			item.Handling = "dbscope=sid DAOs: sidmerge migrate copies <physical>_<A> into <physical>_<B> (B's mapping); the source stays as a read-only archive. " +
				"Non-sid DAOs of the same logical database must be co-located like any shared database"
		} else {
			item.Handling = "shared: A's and B's deployments must map it to the same physical database on the same Mongo, otherwise the merge is refused (nothing is copied)"
		}
		list.Databases = append(list.Databases, item)
	}
	return list, nil
}
