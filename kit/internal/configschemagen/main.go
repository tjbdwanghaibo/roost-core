// Command configschemagen 把 app 与 kit 各 Mod 的配置声明快照成生成器的数据文件
// codegen/internal/roost/kitconfig_gen.go（维护者决定 A4 ①）。
//
// 生成器不导入它生成的运行时（根包 TestCoreDependencyBoundary），读不到 kit Mod 的 ConfigSchema；kit 可以，
// 所以由这里把声明写成 codegen 包里的 Go 字面量。生成器按它渲染配置段、doctor 按它检查工程配置。
// 声明改了没有重新生成时：`go generate ./...` 之后 porcelain 不干净，codegen 的
// TestKitConfigSchemasMatchKitDeclarations（运行本程序 -check）变红。
//
//	go run ./kit/internal/configschemagen -out codegen/internal/roost/kitconfig_gen.go
//	go run ./kit/internal/configschemagen -out codegen/internal/roost/kitconfig_gen.go -check
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	kitconfigdata "github.com/tjbdwanghaibo/roost-core/kit/configdata"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/kit/dataengine"
	kitetcd "github.com/tjbdwanghaibo/roost-core/kit/etcd"
	kitmongo "github.com/tjbdwanghaibo/roost-core/kit/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/kit/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/kit/nest"
	kitops "github.com/tjbdwanghaibo/roost-core/kit/ops"
	kitredis "github.com/tjbdwanghaibo/roost-core/kit/redis"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	kitsaga "github.com/tjbdwanghaibo/roost-core/kit/saga"
	"github.com/tjbdwanghaibo/roost-core/kit/service/account"
	"github.com/tjbdwanghaibo/roost-core/kit/service/chat"
	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global/activity"
	"github.com/tjbdwanghaibo/roost-core/kit/service/mail"
	"github.com/tjbdwanghaibo/roost-core/kit/service/match"
	"github.com/tjbdwanghaibo/roost-core/kit/service/platform"
	"github.com/tjbdwanghaibo/roost-core/kit/service/rank"
	"github.com/tjbdwanghaibo/roost-core/kit/service/session"
	kitstatslog "github.com/tjbdwanghaibo/roost-core/kit/statslog"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/kit/syncbus"
)

// groups 是生成器认识的单元：App 自己、catalog.go 的 Mod 名（dataengine 段同时写 nest 与持久化的键，
// remote_entity 段同时写 Mirror 的停机预算）、framework_services.go 的服务名，以及服务的 RPC 客户端
// （“<服务>.client”，业务服务调用框架服务时注册）。同一单元里的几份声明先合并，冲突即失败。
func groups() map[string][]app.ConfigSchema {
	return map[string][]app.ConfigSchema{
		"app":           {app.AppConfigSchema()},
		"ops":           {kitops.NewOpsMod().ConfigSchema()},
		"statslog":      {kitstatslog.NewStatsLogMod().ConfigSchema()},
		"configdata":    {kitconfigdata.NewConfigDataMod().ConfigSchema()},
		"etcd":          {kitetcd.NewEtcdMod().ConfigSchema()},
		"redis":         {kitredis.NewRedisMod().ConfigSchema()},
		"mongo":         {kitmongo.NewMongoMod().ConfigSchema()},
		"nats":          {kitnats.NewNatsMod(nil).ConfigSchema()},
		"syncbus":       {kitsyncbus.NewSyncBusMod(0).ConfigSchema()},
		"remote_entity": {kitremoteentity.NewRemoteEntityMod(0).ConfigSchema(), kitremoteentity.NewRemoteMirrorMod(0).ConfigSchema()},
		"dataengine":    {kitnest.NewMod(nil).ConfigSchema(), kitdataengine.NewMod().ConfigSchema()},
		"saga":          {kitsaga.NewMod().ConfigSchema()},

		"account":   {new(account.Mod).ConfigSchema()},
		"chat":      {new(chat.Mod).ConfigSchema()},
		"directory": {new(directory.Mod).ConfigSchema()},
		"global":    {new(global.Mod).ConfigSchema()},
		"activity":  {new(activity.Mod).ConfigSchema()},
		"mail":      {new(mail.Mod).ConfigSchema()},
		"match":     {new(match.Mod).ConfigSchema()},
		"platform":  {new(platform.Mod).ConfigSchema()},
		"rank":      {new(rank.Mod).ConfigSchema()},
		"session":   {new(session.Mod).ConfigSchema()},

		"account.client":  {new(account.ClientMod).ConfigSchema()},
		"chat.client":     {new(chat.ClientMod).ConfigSchema()},
		"global.client":   {new(global.ClientMod).ConfigSchema()},
		"activity.client": {new(activity.ClientMod).ConfigSchema()},
		"mail.client":     {new(mail.ClientMod).ConfigSchema()},
		"match.client":    {new(match.ClientMod).ConfigSchema()},
		"platform.client": {new(platform.ClientMod).ConfigSchema()},
		"rank.client":     {new(rank.ClientMod).ConfigSchema()},
		"session.client":  {new(session.ClientMod).ConfigSchema()},
	}
}

func main() {
	out := flag.String("out", "", "file to write (codegen/internal/roost/kitconfig_gen.go)")
	check := flag.Bool("check", false, "compare with -out instead of writing it; exit 1 when it is stale")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "configschemagen: -out is required")
		os.Exit(2)
	}
	source, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configschemagen:", err)
		os.Exit(1)
	}
	if *check {
		current, err := os.ReadFile(*out)
		if err != nil || !bytes.Equal(current, source) {
			fmt.Fprintf(os.Stderr, "configschemagen: %s is not generated from the current kit declarations; run go generate ./codegen/internal/roost\n", *out)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*out, source, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "configschemagen:", err)
		os.Exit(1)
	}
}

func render() ([]byte, error) {
	all := groups()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString("// Code generated by kit/internal/configschemagen from the app and kit Mods' config declarations. DO NOT EDIT.\n\n")
	b.WriteString("package roost\n\nimport \"github.com/tjbdwanghaibo/roost-core/internal/configschema\"\n\n")
	b.WriteString("// kitConfigSchemas is every framework config declaration the generator knows, by unit: \"app\", the\n")
	b.WriteString("// catalog.go Mod names, the framework service names and \"<service>.client\" for their RPC clients.\n")
	b.WriteString("var kitConfigSchemas = map[string]configschema.Schema{\n")
	for _, name := range names {
		merged, err := configschema.Merge(all[name]...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(&b, "%q: {Keys: []configschema.Key{\n", name)
		for _, key := range merged.Keys {
			b.WriteString(keyLiteral(key))
		}
		b.WriteString("}},\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

// keyLiteral 写一个键的字面量，只写非零字段（快照的 diff 才看得清）。
func keyLiteral(key configschema.Key) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{Name: %q, Kind: %q", key.Name, string(key.Kind))
	for _, field := range []struct{ name, value string }{
		{"Default", key.Default}, {"Min", key.Min}, {"Max", key.Max}, {"Help", key.Help},
	} {
		if field.value != "" {
			fmt.Fprintf(&b, ", %s: %q", field.name, field.value)
		}
	}
	if key.Starter {
		fmt.Fprintf(&b, ", Starter: true, Example: %q", key.Example)
	}
	if len(key.Enum) > 0 {
		fmt.Fprintf(&b, ", Enum: %#v", key.Enum)
	}
	for _, flag := range []struct {
		name string
		on   bool
	}{{"Required", key.Required}, {"Secret", key.Secret}, {"SecretOptional", key.SecretOptional}, {"Closed", key.Closed}} {
		if flag.on {
			fmt.Fprintf(&b, ", %s: true", flag.name)
		}
	}
	b.WriteString("},\n")
	return b.String()
}
