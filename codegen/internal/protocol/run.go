package protocol

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/internal/project"
)

// Default project layout the protocol generator reads and writes, and the two
// import-path suffixes its generated code refers to. `roost generate` and the
// project templates refer to these instead of repeating the literals (U-0118).
const (
	DefaultDefDir           = "./protocol/def"
	DefaultBindDir          = "./protocol/player_bind"
	DefaultHandlerDir       = "./game/protocol_handlers"
	PlayerAgentImportSuffix = "/game/player_agent"
	PBImportSuffix          = "/protocol/pb"
)

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("protocol", flag.ContinueOnError)
	flags.SetOutput(stdout)
	defDir := flags.String("def", DefaultDefDir, "protocol definition directory")
	protoDir := flags.String("proto", "./protocol/proto", "generated proto directory")
	pbDir := flags.String("pb", "./protocol/pb", "generated pb go directory")
	msgIDDir := flags.String("msgid", "./protocol/msgid", "generated msg id directory")
	bindDir := flags.String("bind", DefaultBindDir, "generated player_agent binding directory")
	handlerDir := flags.String("handlers", DefaultHandlerDir, "generated player protocol handler directory")
	handlerBootstrap := flags.String("handler-bootstrap", "", "generated aggregate player protocol registration file")
	robotProtocolFile := flags.String("robot-protocol", "./service/robot/protocol/registry_gen.go", "generated robot protocol registry path")
	manifestFile := flags.String("manifest", "./protocol/protocol_manifest.json", "generated protocol manifest path")
	reverseProtoFile := flags.String("reverse-proto", "", "reverse-generate Go protocol defs from a proto file")
	reverseOutDir := flags.String("reverse-out", "./protocol/def/imported", "reverse-generated Go def output directory")
	reversePackage := flags.String("reverse-package", "def", "reverse-generated Go package name")
	force := flags.Bool("force", false, "force regeneration")
	if err := flags.Parse(args); err != nil {
		return err
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
	if len(defs.Structs) == 0 && *handlerBootstrap == "" {
		_, _ = fmt.Fprintf(stdout, "no protocol structs found in %s\n", *defDir)
		return nil
	}
	projectInfo, err := project.Discover(*defDir)
	if err != nil {
		return fmt.Errorf("discover target module: %w", err)
	}
	defs.ModulePath = projectInfo.ModulePath
	defs.GoPackage = defs.ModulePath + "/protocol/pb;pb"
	if len(defs.Structs) == 0 {
		if err := os.MkdirAll(filepath.Dir(*handlerBootstrap), 0o755); err != nil {
			return err
		}
		content, err := generateProtocolBootstrap(defs, "")
		if err != nil {
			return err
		}
		changed, err := writeIfChanged(*handlerBootstrap, content, *force)
		if err != nil {
			return err
		}
		printChange(stdout, *handlerBootstrap, changed)
		return nil
	}
	dirs := []string{*protoDir, *pbDir, *msgIDDir, filepath.Dir(*manifestFile)}
	if *bindDir != "" {
		dirs = append(dirs, *bindDir)
	}
	if *robotProtocolFile != "" {
		dirs = append(dirs, filepath.Dir(*robotProtocolFile))
	}
	if *handlerBootstrap != "" {
		dirs = append(dirs, filepath.Dir(*handlerBootstrap))
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
	if *handlerDir == "" {
		if *handlerBootstrap == "" {
			return nil
		}
		content, err := generateProtocolBootstrap(defs, "")
		if err != nil {
			return err
		}
		changed, err := writeIfChanged(*handlerBootstrap, content, *force)
		if err != nil {
			return err
		}
		printChange(stdout, *handlerBootstrap, changed)
		return nil
	}
	files, err := generateProtocolHandlerFiles(defs)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		path := filepath.Join(*handlerDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		changed, err := writeIfChanged(path, files[rel], *force)
		if err != nil {
			return err
		}
		printChange(stdout, path, changed)
	}
	if *handlerBootstrap != "" {
		handlerImport, err := projectInfo.ImportPath(*handlerDir)
		if err != nil {
			return fmt.Errorf("resolve protocol handler import: %w", err)
		}
		content, err := generateProtocolBootstrap(defs, handlerImport)
		if err != nil {
			return err
		}
		changed, err := writeIfChanged(*handlerBootstrap, content, *force)
		if err != nil {
			return err
		}
		printChange(stdout, *handlerBootstrap, changed)
	}
	return nil
}

func printChange(w io.Writer, path string, changed bool) {
	state := "unchanged"
	if changed {
		state = "generated"
	}
	_, _ = fmt.Fprintf(w, "%s: %s\n", state, path)
}
