package roost

import (
	"fmt"
	"sort"
	"strings"
)

type modSpec struct {
	ImportPath  string
	Alias       string
	Constructor string
	Depends     []string
	DevService  string
}

//go:generate go run ../../../wiring/internal/configschemagen -out kitconfig_gen.go

// modConfigSection is the configuration section a catalog Mod writes into a
// service config: the keys its declaration marks with an example (the starter
// keys), with the declaration's help as comments (maintainer decision).
// The declarations are the wiring Mods' own, snapshotted into kitconfig_gen.go;
// a Mod with no declared starter keys writes nothing. Examples that name a
// shared resource carry the project's name placeholders (projectNameVars).
func modConfigSection(name, project string) string {
	return kitConfigSchemas[name].StarterYAML(projectNameVars(project))
}

// frameworkConfigSection is the same for a hosted framework service; its key
// prefix example names the project.
func frameworkConfigSection(name, project string) string {
	return kitConfigSchemas[name].StarterYAML(projectNameVars(project))
}

// projectNameVars 是声明 example 里的工程名占位符。决定“与谁共享”的名字（总线主题前缀、Data Engine / saga /
// Remote 的库、effects / saga 的主题与流、etcd 注册前缀、Redis 键前缀）在生成配置里按工程名派生，两个工程共用
// 一套开发用 Mongo / NATS / etcd 时不撞库撞流：
//   - {project}：工程名本身（已满足 ^[a-z][a-z0-9_]*$，是合法的 NATS 主题 token），用于主题前缀与键前缀；
//   - {PROJECT}：工程名大写，用于 JetStream 流名（沿用 ROOST_EFFECTS 一类的大写写法）；
//   - {project_db}：Mongo 库名，与 DAO 骨架的 db= 同一规则（daoDatabaseName：系统库加 _data、截到 52 字节），
//     saga / Remote 的库在它后面加 _saga / _remote（仍不超过 Mongo 63 字节上限）。
//
// 只影响生成器写出的配置；Mod 的运行期缺省值（default）不变。已有工程的配置归业务，生成器不改写已有的值。
func projectNameVars(project string) map[string]string {
	return map[string]string{
		"project":    project,
		"PROJECT":    strings.ToUpper(project),
		"project_db": daoDatabaseName(project),
	}
}

// defaultConfigDataDir is the generated config_data.dir: relative, resolved
// against the process working directory (the project root in development,
// WORKDIR /app in the generated image). The Dockerfile copies the data to the
// same relative path under /app, so both come from here.
const defaultConfigDataDir = "configs/data"

// defaultStatsLogDir is the generated stats_log.dir: relative like the config
// data, so it lands under the working directory — WORKDIR /app in the image,
// the release directory under systemd. Every generated deployment gives that
// path a writable mount or link, so they come from here too.
const defaultStatsLogDir = "log"

var modCatalog = map[string]modSpec{
	"lock": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/lock", Alias: "kitlock", Constructor: "kitlock.NewLockMod()",
	},
	"ops": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/ops", Alias: "kitops", Constructor: "kitops.NewOpsMod()",
	},
	"statslog": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/statslog", Alias: "kitstatslog", Constructor: "kitstatslog.NewStatsLogMod()",
	},
	"configdata": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/configdata", Alias: "kitconfigdata", Constructor: "kitconfigdata.NewConfigDataMod()",
	},
	"etcd": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/etcd", Alias: "kitetcd", Constructor: "kitetcd.NewEtcdMod()",
		DevService: "etcd",
	},
	"redis": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/redis", Alias: "kitredis", Constructor: "kitredis.NewRedisMod()",
		DevService: "redis",
	},
	"mongo": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/mongo", Alias: "kitmongo", Constructor: "kitmongo.NewMongoMod()",
		DevService: "mongo",
	},
	"nats": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/nats", Alias: "kitnats", Constructor: "kitnats.NewNatsMod(nil)",
		DevService: "nats",
	},
	"syncbus": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/syncbus", Alias: "kitsyncbus", Constructor: "kitsyncbus.NewSyncBusMod(0)", Depends: []string{"nats"},
	},
	"remote_entity": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/remoteentity", Alias: "kitremoteentity", Depends: []string{"redis", "mongo", "syncbus"},
	},
	"dataengine": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/dataengine", Alias: "kitdataengine", Depends: []string{"mongo", "nats"},
	},
	"manager": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/manager", Alias: "kitmanager",
	},
	"nest": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/nest", Alias: "kitnest",
	},
	// heartbeat 是实体心跳驱动（gameplay/heartbeat，D20）：投递走同一服务的 Nest，所以依赖 nest；驱动是工程的
	// game/heartbeat.Driver（生成），roost add heartbeat / roost add behavior 自动给服务加上这个 Mod。
	"heartbeat": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/heartbeat", Alias: "kitheartbeat", Depends: []string{"nest"},
	},
	"saga": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/saga", Alias: "kitsaga", Constructor: "kitsaga.NewMod()", Depends: []string{"mongo", "nats"},
	},
	// web 监听 roost add webroute 生成的业务 HTTP 路由；服务在 Init 里 kitweb.RegisterRoutes(registry, &web.Service{})。
	"web": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/web", Alias: "kitweb", Constructor: "kitweb.NewMod()",
	},
}

var knownFeatures = map[string]bool{
	"protocol": true, "config": true, "entity": true, "nest": true,
	"dao": true, "attribute": true, "webroute": true, "rpc": true,
	"errcode":           true,
	"saga":              true,
	"nettransport-quic": true, "nettransport-kcp": true, "nettransport-udp": true,
}

func resolveMods(requested []string) ([]string, error) {
	requested = append([]string(nil), requested...)
	needsPersistence := contains(requested, "nest") || contains(requested, "saga")
	if needsPersistence && !contains(requested, "dataengine") {
		requested = append(requested, "dataengine")
	}
	seen := map[string]bool{}
	visiting := map[string]bool{}
	var out []string
	var visit func(string) error
	visit = func(name string) error {
		spec, ok := modCatalog[name]
		if !ok {
			return fmt.Errorf("unknown wiring mod %q", name)
		}
		if seen[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("wiring mod dependency cycle at %q", name)
		}
		visiting[name] = true
		for _, dependency := range spec.Depends {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[name] = false
		seen[name] = true
		out = append(out, name)
		return nil
	}
	for _, name := range requested {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func allProjectMods(m Manifest) []string {
	requested := append([]string(nil), m.SharedMods...)
	for name := range m.Services {
		requested = append(requested, effectiveServiceMods(m, name)...)
	}
	resolved, err := resolveMods(requested)
	if err == nil {
		return resolved
	}
	// Manifest validation reports the actual catalog error. Keep rendering
	// deterministic for callers that are collecting more than one diagnostic.
	seen := map[string]bool{}
	for _, mod := range requested {
		seen[mod] = true
	}
	out := make([]string, 0, len(seen))
	for mod := range seen {
		out = append(out, mod)
	}
	sort.Strings(out)
	return out
}
