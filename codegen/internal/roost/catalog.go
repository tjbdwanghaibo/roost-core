package roost

import (
	"fmt"
	"sort"
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
// keys), with the declaration's help as comments (maintainer decision A4 ①).
// The declarations are the kit Mods' own, snapshotted into kitconfig_gen.go;
// a Mod with no declared starter keys writes nothing.
func modConfigSection(name string) string {
	return kitConfigSchemas[name].StarterYAML(nil)
}

// frameworkConfigSection is the same for a hosted framework service; its key
// prefix example names the project.
func frameworkConfigSection(name, project string) string {
	return kitConfigSchemas[name].StarterYAML(map[string]string{"project": project})
}

// defaultConfigDataDir is the generated config_data.dir: relative, resolved
// against the process working directory (the project root in development,
// WORKDIR /app in the generated image). The Dockerfile copies the data to the
// same relative path under /app (RR-20260927-34), so both come from here.
const defaultConfigDataDir = "configs/data"

// defaultStatsLogDir is the generated stats_log.dir: relative like the config
// data, so it lands under the working directory — WORKDIR /app in the image,
// the release directory under systemd. Every generated deployment gives that
// path a writable mount or link (RR-20260928-04), so they come from here too.
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
	"saga": {
		ImportPath: "github.com/tjbdwanghaibo/roost-core/wiring/saga", Alias: "kitsaga", Constructor: "kitsaga.NewMod()", Depends: []string{"mongo", "nats"},
	},
}

var knownFeatures = map[string]bool{
	"protocol": true, "config": true, "entity": true, "nest": true,
	"event": true, "dao": true, "attribute": true, "webroute": true, "rpc": true,
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
			return fmt.Errorf("unknown kit mod %q", name)
		}
		if seen[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("kit mod dependency cycle at %q", name)
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
