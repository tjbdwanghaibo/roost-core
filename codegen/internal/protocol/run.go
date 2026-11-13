package protocol

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/project"
)

// Default project layout the protocol generator reads and writes, and the two
// import-path suffixes its generated code refers to. `roost generate` and the
// project templates refer to these instead of repeating the literals.
const (
	DefaultDefDir  = "./protocol/def"
	DefaultBindDir = "./protocol/player_bind"
	// DefaultHandlerFile 是玩家接入层的协议登记文件（controller 接口、各功能注册、总入口与推送编码器），
	// 包名取所在目录名。`roost generate` 在启用 access.player 时传它；单独跑 protocol 生成器时缺省不写。
	DefaultHandlerFile = "./internal/access/player/protocols_gen.go"
	// PlayerAccessImport 是协议派发层的 import 路径（2026-11-13 起是 core 的框架包，不再生成进工程）。
	PlayerAccessImport = "github.com/tjbdwanghaibo/roost-core/infra/network/playeraccess"
	PBImportSuffix     = "/protocol/pb"
)

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("protocol", flag.ContinueOnError)
	flags.SetOutput(stdout)
	defDir := flags.String("def", DefaultDefDir, "protocol definition directory")
	protoDir := flags.String("proto", "./protocol/proto", "generated proto directory")
	pbDir := flags.String("pb", "./protocol/pb", "generated pb go directory")
	msgIDDir := flags.String("msgid", "./protocol/msgid", "generated msg id directory")
	bindDir := flags.String("bind", DefaultBindDir, "generated playeraccess binding directory")
	handlerFile := flags.String("handlers", "", "generated player protocol registration file (controller interfaces, per-feature and aggregate registration), e.g. "+DefaultHandlerFile)
	controllerDir := flags.String("controllers", "", "create missing application-owned controller scaffolds")
	robotProtocolFile := flags.String("robot-protocol", "./loadtest/protocol/registry_gen.go", "generated robot protocol registry path")
	manifestFile := flags.String("manifest", "./protocol/protocol_manifest.json", "generated protocol manifest path")
	csharpFile := flags.String("csharp", "", "optional generated C# message id file; PB classes use protoc on the generated proto")
	reverseProtoFile := flags.String("reverse-proto", "", "reverse-generate Go protocol defs from a proto file")
	reverseOutDir := flags.String("reverse-out", "./protocol/def/imported", "reverse-generated Go def output directory")
	reversePackage := flags.String("reverse-package", "def", "reverse-generated Go package name")
	force := flags.Bool("force", false, "force regeneration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// 空定义的退役也要约束扩展名，不能让-csharp指向另一个Go生成产物。
	if *csharpFile != "" && !strings.EqualFold(filepath.Ext(*csharpFile), ".cs") {
		return fmt.Errorf("C# message id output must end in .cs: %s", *csharpFile)
	}

	if *reverseProtoFile != "" {
		content, err := generateReverseGoFromProto(*reverseProtoFile, *reversePackage)
		if err != nil {
			return fmt.Errorf("reverse proto %s: %w", *reverseProtoFile, err)
		}
		if err := os.MkdirAll(*reverseOutDir, 0o755); err != nil {
			return err
		}
		out := filepath.Join(*reverseOutDir, "gen_proto_"+toSnake(strings.TrimSuffix(filepath.Base(*reverseProtoFile), filepath.Ext(*reverseProtoFile)))+".go")
		changed, err := writeIfChanged(out, content, *force)
		if err != nil {
			return err
		}
		printChange(stdout, out, changed)
		return nil
	}

	defs, err := parseDefDir(*defDir)
	if err != nil {
		return fmt.Errorf("parse protocol defs: %w", err)
	}
	if len(defs.Structs) == 0 {
		outputs := protocolOutputPaths{
			csharp:   *csharpFile,
			proto:    filepath.Join(*protoDir, "protocol.proto"),
			pb:       filepath.Join(*pbDir, "protocol.pb.go"),
			msgID:    filepath.Join(*msgIDDir, "msgid_gen.go"),
			bind:     *bindDir,
			robot:    *robotProtocolFile,
			manifest: *manifestFile,
		}
		if err := retireProtocolOutputs(outputs, stdout); err != nil {
			return err
		}
		// 没有协议时登记文件照样写（空的总入口），接入层 Mod 仍能编译。
		if err := writeProtocolRegistration(*handlerFile, defs, *force, stdout); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "no protocol structs found in %s\n", *defDir)
		return nil
	}
	projectInfo, err := project.Discover(*defDir)
	if err != nil {
		return fmt.Errorf("discover target module: %w", err)
	}
	defs.ModulePath = projectInfo.ModulePath
	defs.GoPackage = defs.ModulePath + "/protocol/pb;pb"
	if err := validateCSharpOutput(*csharpFile); err != nil {
		return err
	}
	dirs := []string{*protoDir, *pbDir, *msgIDDir, filepath.Dir(*manifestFile)}
	if *csharpFile != "" {
		dirs = append(dirs, filepath.Dir(*csharpFile))
	}
	if *bindDir != "" {
		dirs = append(dirs, *bindDir)
	}
	if *robotProtocolFile != "" {
		dirs = append(dirs, filepath.Dir(*robotProtocolFile))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	writes := []struct {
		path string
		fn   func(*Definitions) ([]byte, error)
	}{
		{filepath.Join(*protoDir, "protocol.proto"), generateProto},
		{filepath.Join(*pbDir, "protocol.pb.go"), generatePBGo},
		{filepath.Join(*msgIDDir, "msgid_gen.go"), generateMsgIDGo},
		{*manifestFile, generateManifest},
	}
	if *bindDir != "" {
		writes = append(writes, struct {
			path string
			fn   func(*Definitions) ([]byte, error)
		}{filepath.Join(*bindDir, "bind_gen.go"), generateBindGo})
	}
	if *csharpFile != "" {
		writes = append(writes, struct {
			path string
			fn   func(*Definitions) ([]byte, error)
		}{*csharpFile, generateCSharpIDs})
	}
	if *robotProtocolFile != "" {
		writes = append(writes, struct {
			path string
			fn   func(*Definitions) ([]byte, error)
		}{*robotProtocolFile, generateRobotProtocolGo})
	}
	for _, item := range writes {
		content, err := item.fn(defs)
		if err != nil {
			return fmt.Errorf("generate %s: %w", item.path, err)
		}
		changed, err := writeIfChanged(item.path, content, *force)
		if err != nil {
			return fmt.Errorf("write %s: %w", item.path, err)
		}
		printChange(stdout, item.path, changed)
	}
	if *controllerDir != "" {
		if err := generateControllers(*controllerDir, defs, stdout); err != nil {
			return err
		}
	}
	return writeProtocolRegistration(*handlerFile, defs, *force, stdout)
}

// writeProtocolRegistration 写接入层的协议登记文件；path 为空时不写（工程没有玩家接入层）。
func writeProtocolRegistration(path string, defs *Definitions, force bool, stdout io.Writer) error {
	if path == "" {
		return nil
	}
	content, err := generateProtocolRegistration(defs, filepath.Base(filepath.Dir(path)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	changed, err := writeIfChanged(path, content, force)
	if err != nil {
		return err
	}
	printChange(stdout, path, changed)
	return nil
}

func printChange(w io.Writer, path string, changed bool) {
	state := "unchanged"
	if changed {
		state = "generated"
	}
	_, _ = fmt.Fprintf(w, "%s: %s\n", state, path)
}
