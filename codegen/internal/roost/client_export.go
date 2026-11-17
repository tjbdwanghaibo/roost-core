package roost

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/dao"
	codeerr "github.com/tjbdwanghaibo/roost-core/codegen/internal/errcode"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
)

// 前后端公共协议目录（roost.yaml 的 client_export，维护者 2026-11-17，见
// docs/maintenance/FEATURE-2026-11-17-client-export.md）。声明了它的工程，roost generate 每次把客户端要的协议产物
// 整份写进 <dir>：proto 副本、protoc 生成的 C#、Go 生成的消息号 / 错误码 / DAO 补丁应用代码和 manifest.json。
// 没声明的工程不跑这一步，生成结果与之前逐字节相同。

// DefaultClientExportDir 是 client_export.dir 的缺省值。
const DefaultClientExportDir = "shared/protocol"

// sdkGoogleProtobufVersion 是 C# SDK（client/dotnet/Roost.Client.Codecs/Roost.Client.Codecs.csproj）锁定的
// Google.Protobuf 版本：生成器不能读仓库里的 csproj，这里是副本，TestSDKGoogleProtobufVersionMatchesCodecs
// 核对两处一致。所需的 protoc 版本由它推出（见 requiredProtocVersion），不另写一份。
const sdkGoogleProtobufVersion = "3.31.1"

// clientExportKind 写在 manifest.json 里，roost client sync 凭它认出导出清单。
const clientExportKind = "roost-client-export"

const clientExportSchema = 1

// 导出目录里归生成器所有的文件（相对 <dir>）。同名文件每次整份重写；本轮不产出的（例如没有 DAO 同步字段时的
// DaoSync.cs、去掉 csharp 后的全部 .cs）删掉。目录里别的文件不动。
const (
	exportProtocolProto = "proto/protocol.proto"
	exportDaoSyncProto  = "proto/dao_sync.proto"
	exportProtocolCS    = "csharp/Protocol.cs"
	exportDaoSyncCS     = "csharp/DaoSync.cs"
	exportDaoSyncApply  = "csharp/DaoSyncApply.cs"
	exportMessageIDs    = "csharp/MessageIds.cs"
	exportErrorCodes    = "csharp/ErrorCodes.cs"
	exportManifest      = "manifest.json"
)

var clientExportFiles = []string{
	exportProtocolProto, exportDaoSyncProto,
	exportProtocolCS, exportDaoSyncCS, exportDaoSyncApply, exportMessageIDs, exportErrorCodes,
	exportManifest,
}

// 缺省命名空间：protoc 按 proto package 推出 Roost.Protocol / Roost.Dao.Sync；消息号与错误码沿用 -csharp 的
// Roost.Generated。配了 csharp.namespace 时：协议、消息号、错误码都在 <namespace>，DAO 同步在 <namespace>.DaoSync。
const (
	defaultProtocolNamespace = "Roost.Protocol"
	defaultDaoSyncNamespace  = "Roost.Dao.Sync"
)

// ClientExportSpec 是 roost.yaml 的 client_export。
type ClientExportSpec struct {
	// Protocol 是协议导出的显式开关：写 false 表示不导出协议（只导出配置时用，不找 protoc，doctor 也不查协议导出）；
	// 不写或写 true 照常导出。false 时不能再写 dir、languages、csharp。
	Protocol *bool `yaml:"protocol,omitempty"`
	// Dir 相对工程根，缺省 shared/protocol；产物提交进仓库。
	Dir string `yaml:"dir,omitempty"`
	// Languages 目前只支持 csharp；为空时只导出 proto 副本与 manifest.json。
	Languages []string            `yaml:"languages,omitempty"`
	CSharp    *ClientExportCSharp `yaml:"csharp,omitempty"`
	// Config 是配置表导出（client_export.config，见 client_config_export.go）；不写就不导出配置。
	Config *ClientConfigExportSpec `yaml:"config,omitempty"`

	// projectRoot 是读 roost.yaml 时的工程根（绝对路径），用来解析相对的 csharp.protoc：生成在暂存树里跑，
	// 暂存树不保留可执行位。Not part of roost.yaml.
	projectRoot string
}

// ClientExportCSharp 是 client_export.csharp。
type ClientExportCSharp struct {
	// Namespace 缺省为空：协议用 proto package 推出的 Roost.Protocol，DAO 同步 Roost.Dao.Sync，消息号与错误码 Roost.Generated。
	Namespace string `yaml:"namespace,omitempty"`
	// Protoc 是 protoc 的路径（相对路径按工程根解析）；不写时依次找环境变量 ROOST_PROTOC 与 PATH。
	Protoc string `yaml:"protoc,omitempty"`
}

var csharpNamespacePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// dir 是规范化后的导出目录（斜杠分隔）。
func (spec *ClientExportSpec) dir() string {
	if spec == nil || strings.TrimSpace(spec.Dir) == "" {
		return DefaultClientExportDir
	}
	return path.Clean(filepath.ToSlash(strings.TrimSpace(spec.Dir)))
}

func (spec *ClientExportSpec) csharp() bool {
	return spec.protocol() && contains(spec.Languages, "csharp")
}

// protocol 判断是否导出协议：只看显式开关 client_export.protocol，写 false 才不导出（维护者 2026-11-17：不从别的键推断，
// 已有的 client_export 加上 config 后协议照常导出）。
func (spec *ClientExportSpec) protocol() bool {
	return spec != nil && (spec.Protocol == nil || *spec.Protocol)
}

func (spec *ClientExportSpec) namespaces() clientExportNamespaces {
	if spec.CSharp != nil && spec.CSharp.Namespace != "" {
		ns := spec.CSharp.Namespace
		return clientExportNamespaces{Protocol: ns, DaoSync: ns + ".DaoSync", IDs: ns}
	}
	return clientExportNamespaces{Protocol: defaultProtocolNamespace, DaoSync: defaultDaoSyncNamespace, IDs: protocol.DefaultCSharpIDsNamespace}
}

func (spec *ClientExportSpec) validate() error {
	if spec == nil {
		return nil
	}
	joined := validateExportDir("client_export.dir", spec.Dir, DefaultClientExportDir)
	if !spec.protocol() {
		var set []string
		if strings.TrimSpace(spec.Dir) != "" {
			set = append(set, "dir")
		}
		if len(spec.Languages) > 0 {
			set = append(set, "languages")
		}
		if spec.CSharp != nil {
			set = append(set, "csharp")
		}
		if len(set) > 0 {
			joined = errors.Join(joined, fmt.Errorf("client_export.protocol is false but client_export also sets %s, which only the protocol export reads; remove them or the protocol: false line", strings.Join(set, ", ")))
		}
		if spec.Config == nil {
			joined = errors.Join(joined, errors.New("client_export.protocol is false and client_export.config is not declared, so nothing is exported; remove client_export or declare config"))
		}
	}
	seen := map[string]bool{}
	for _, language := range spec.Languages {
		if language != "csharp" {
			joined = errors.Join(joined, fmt.Errorf("client_export.languages has unsupported language %q; supported: csharp", language))
		}
		if seen[language] {
			joined = errors.Join(joined, fmt.Errorf("client_export.languages repeats %q", language))
		}
		seen[language] = true
	}
	if spec.CSharp != nil {
		if !seen["csharp"] {
			joined = errors.Join(joined, errors.New("client_export.csharp is set but client_export.languages does not list csharp"))
		}
		if ns := spec.CSharp.Namespace; ns != "" && !csharpNamespacePattern.MatchString(ns) {
			joined = errors.Join(joined, fmt.Errorf("client_export.csharp.namespace %q is not a C# namespace (dot-separated identifiers)", ns))
		}
	}
	if spec.Config != nil {
		joined = errors.Join(joined, spec.Config.validate())
		if spec.protocol() {
			protocolDir, configDir := spec.dir(), spec.Config.dir()
			if protocolDir == configDir || strings.HasPrefix(configDir, protocolDir+"/") || strings.HasPrefix(protocolDir, configDir+"/") {
				joined = errors.Join(joined, fmt.Errorf("client_export.config.dir %q overlaps client_export.dir %q; the protocol and config exports each own their directory (both write manifest.json)", configDir, protocolDir))
			}
		}
	}
	return joined
}

// validateExportDir 核对一个导出目录（client_export.dir、client_export.config.dir）：相对工程根、在工程内、
// 不用生成时跳过的目录名、不在服务端源码与生成物的目录下。raw 为空表示用缺省值，缺省值本身合法。
func validateExportDir(key, raw, fallback string) error {
	var joined error
	slashed := filepath.ToSlash(strings.TrimSpace(raw))
	dir := fallback
	if slashed != "" {
		dir = path.Clean(slashed)
	}
	switch {
	case slashed != "" && (path.IsAbs(slashed) || filepath.IsAbs(raw) || filepath.VolumeName(raw) != ""):
		joined = errors.Join(joined, fmt.Errorf("%s %q must be relative to the project root", key, raw))
	case dir == "." || dir == ".." || strings.HasPrefix(dir, "../"):
		joined = errors.Join(joined, fmt.Errorf("%s %q must be a directory inside the project", key, raw))
	default:
		for _, segment := range strings.Split(dir, "/") {
			if skippedProjectDirectory(segment) {
				// copyProject / 提交计划都跳过这些名字，生成的文件进不了工程。
				joined = errors.Join(joined, fmt.Errorf("%s %q uses directory name %q, which generation skips", key, raw, segment))
			}
		}
		top, _, _ := strings.Cut(dir, "/")
		switch top {
		case "protocol", "db", "game", "internal", "service", "configs", "cmd", "deploy":
			joined = errors.Join(joined, fmt.Errorf("%s %q is inside %s/, which holds server sources and generator output; use a directory of its own such as %s", key, raw, top, fallback))
		}
	}
	return joined
}

// owns 判断工程相对路径 rel（斜杠分隔）是不是导出目录（协议与配置）里归生成器所有的文件。没声明 client_export 时为 false。
func (spec *ClientExportSpec) owns(rel string) bool {
	if spec == nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if spec.Config.owns(rel) {
		return true
	}
	if !spec.protocol() {
		return false
	}
	inner, ok := strings.CutPrefix(rel, spec.dir()+"/")
	if !ok {
		return false
	}
	for _, name := range clientExportFiles {
		if inner == name {
			return true
		}
	}
	return false
}

// requiredProtocVersion 由 Google.Protobuf 的版本推出 protoc 的版本：protobuf 自 v21 起 protoc 写 <minor>.<patch>，
// C# 运行库写 3.<minor>.<patch>（3.31.1 对 protoc 31.1）。
func requiredProtocVersion() string {
	return strings.TrimPrefix(sdkGoogleProtobufVersion, "3.")
}

func protocInstallHint(version string) string {
	return fmt.Sprintf("install protoc %[1]s from https://github.com/protocolbuffers/protobuf/releases/tag/v%[1]s (protoc-%[1]s-<os>-<arch>.zip), "+
		"then point client_export.csharp.protoc or the ROOST_PROTOC environment variable at its bin/protoc, or put it first on PATH", version)
}

// resolveProtoc 按 client_export.csharp.protoc、环境变量 ROOST_PROTOC、PATH 的顺序找 protoc，并要求版本与 SDK 锁定的
// Google.Protobuf 一致。返回路径与它的来源（报错时写明）。
func (spec *ClientExportSpec) resolveProtoc() (string, error) {
	want := requiredProtocVersion()
	var candidate, source string
	switch {
	case spec.CSharp != nil && strings.TrimSpace(spec.CSharp.Protoc) != "":
		candidate, source = strings.TrimSpace(spec.CSharp.Protoc), "client_export.csharp.protoc"
		if !filepath.IsAbs(candidate) && spec.projectRoot != "" {
			candidate = filepath.Join(spec.projectRoot, candidate)
		}
	case os.Getenv("ROOST_PROTOC") != "":
		candidate, source = os.Getenv("ROOST_PROTOC"), "ROOST_PROTOC"
	default:
		found, err := exec.LookPath("protoc")
		if err != nil {
			return "", fmt.Errorf("client_export.languages lists csharp, which needs protoc %s (Google.Protobuf %s is what the Roost C# SDK locks), but none was found in client_export.csharp.protoc, ROOST_PROTOC or PATH; %s",
				want, sdkGoogleProtobufVersion, protocInstallHint(want))
		}
		candidate, source = found, "PATH"
	}
	output, err := exec.Command(candidate, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("client_export needs protoc %s; %s (from %s) did not run: %v; %s", want, candidate, source, err, protocInstallHint(want))
	}
	got := strings.TrimSpace(string(output))
	if strings.TrimPrefix(got, "libprotoc ") != want {
		return "", fmt.Errorf("client_export needs protoc %s to match Google.Protobuf %s locked by the Roost C# SDK, but %s (from %s) reports %q; %s",
			want, sdkGoogleProtobufVersion, candidate, source, got, protocInstallHint(want))
	}
	return candidate, nil
}

type clientExportNamespaces struct {
	Protocol string `json:"protocol"`
	DaoSync  string `json:"dao_sync"`
	IDs      string `json:"ids"`
}

type clientExportSources struct {
	// ProtocolDef 是 protocol/def 下全部 .go（不含 _test.go）的路径与内容的哈希；没有协议时为空。
	ProtocolDef string `json:"protocol_def,omitempty"`
	// DaoSync 是同步字段的哈希：db/def 生成的 db/dao_sync.proto（只由同步字段决定）；没有同步字段时为空。
	DaoSync string `json:"dao_sync,omitempty"`
}

type clientErrorCode struct {
	Code    int32  `json:"code"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// clientExportManifest 是 <dir>/manifest.json。
type clientExportManifest struct {
	Kind           string                  `json:"kind"`
	Schema         int                     `json:"schema"`
	Core           string                  `json:"core"`
	Generator      string                  `json:"generator"`
	Languages      []string                `json:"languages"`
	Protoc         string                  `json:"protoc,omitempty"`
	GoogleProtobuf string                  `json:"google_protobuf,omitempty"`
	Namespaces     *clientExportNamespaces `json:"csharp_namespaces,omitempty"`
	Sources        clientExportSources     `json:"sources"`
	// Files 是除 manifest.json 外每个导出文件的哈希（键相对 <dir>）。
	Files      map[string]string        `json:"files"`
	Messages   []protocol.ClientMessage `json:"messages"`
	ErrorCodes []clientErrorCode        `json:"error_codes"`
}

// contentHash 按 LF 计算：CRLF 检出的同一份文件不算变化（与 generate --check 一致）。
func contentHash(raw []byte) string {
	sum := sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codegenVersion 是正在运行的生成器版本，与 roost version 一致。
func codegenVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "development"
}

// projectCoreVersion 读工程 go.mod 里 require 的 roost-core 版本；读不到时为空。
func projectCoreVersion(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	const module = "github.com/tjbdwanghaibo/roost-core"
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	inRequire := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inRequire = true
			continue
		case inRequire && line == ")":
			inRequire = false
			continue
		}
		fields := strings.Fields(line)
		if !inRequire {
			if len(fields) < 1 || fields[0] != "require" {
				continue
			}
			fields = fields[1:]
		}
		if len(fields) >= 2 && fields[0] == module {
			return fields[1]
		}
	}
	return ""
}

// protocolDefHash 是协议源头的哈希：protocol/def 下全部 .go（不含 _test.go），按路径排序，路径与内容都计入。
func protocolDefHash(root string) (string, error) {
	defDir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(protocol.DefaultDefDir, "./")))
	var files []string
	err := filepath.WalkDir(defDir, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() || filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(defDir, file)
		fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(rel), len(raw))
		hash.Write(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// readOptional 读工程里的一个生成物；不存在时返回 nil。
func readOptional(root, rel string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}

// withCSharpNamespace 在 proto 的 package 行后加 option csharp_namespace；namespace 为空时原样返回。
func withCSharpNamespace(proto []byte, namespace string) ([]byte, error) {
	if namespace == "" {
		return proto, nil
	}
	if bytes.Contains(proto, []byte("option csharp_namespace")) {
		return nil, errors.New("proto already declares option csharp_namespace")
	}
	var out bytes.Buffer
	inserted := false
	for line := range bytes.Lines(proto) {
		out.Write(line)
		if !inserted && bytes.HasPrefix(line, []byte("package ")) {
			fmt.Fprintf(&out, "\noption csharp_namespace = %q;\n", namespace)
			inserted = true
		}
	}
	if !inserted {
		return nil, errors.New("proto has no package line")
	}
	return out.Bytes(), nil
}

// daoSyncApplyNamespace 把 dao 生成器写死的 Roost.Dao.Sync 换成配置的命名空间。
func daoSyncApplyNamespace(raw []byte, namespace string) ([]byte, error) {
	old := []byte("\nnamespace " + defaultDaoSyncNamespace + " {\n")
	if bytes.Count(raw, old) != 1 {
		return nil, fmt.Errorf("db/dao_sync_apply.cs does not declare namespace %s exactly once", defaultDaoSyncNamespace)
	}
	if namespace == defaultDaoSyncNamespace {
		return raw, nil
	}
	return bytes.Replace(raw, old, []byte("\nnamespace "+namespace+" {\n"), 1), nil
}

// runProtocCSharp 在临时目录里对导出的 proto 跑 protoc --csharp_out，返回 proto 文件名 → 生成的 C#。
func runProtocCSharp(protoc string, protos map[string][]byte) (map[string][]byte, error) {
	if len(protos) == 0 {
		return nil, nil
	}
	tmp, err := os.MkdirTemp("", "roost-client-export-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	in, out := filepath.Join(tmp, "proto"), filepath.Join(tmp, "csharp")
	for _, dir := range []string{in, out} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(protos))
	for name, raw := range protos {
		if err := os.WriteFile(filepath.Join(in, name), raw, 0o644); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	args := append([]string{"-I", in, "--csharp_out=" + out}, names...)
	command := exec.Command(protoc, args...)
	command.Dir = tmp
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("protoc --csharp_out: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	results := make(map[string][]byte, len(names))
	for _, name := range names {
		csName := protocCSharpFileName(name)
		raw, err := os.ReadFile(filepath.Join(out, csName))
		if err != nil {
			return nil, fmt.Errorf("protoc did not write %s: %w", csName, err)
		}
		results[name] = raw
	}
	return results, nil
}

// protocCSharpFileName 是 protoc C# 插件的输出文件名：proto 文件名去扩展名后按 _ 分词转 PascalCase。
func protocCSharpFileName(protoName string) string {
	base := strings.TrimSuffix(protoName, ".proto")
	var b strings.Builder
	upper := true
	for _, r := range base {
		if r == '_' || r == '-' || r == '.' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		upper = r >= '0' && r <= '9'
		b.WriteRune(r)
	}
	return b.String() + ".cs"
}

// clientExportInputs 是导出要读的服务端产物与源头（都取自 root 当前的磁盘内容）。
type clientExportInputs struct {
	protocolProto, daoSyncProto, daoSyncApply []byte
	ids                                       protocol.ClientIDs
	errcodes                                  []codeerr.Definition
	sources                                   clientExportSources
}

func readClientExportInputs(root string, m Manifest) (clientExportInputs, error) {
	var in clientExportInputs
	var err error
	if hasFeature(m, "protocol") {
		if in.protocolProto, err = readOptional(root, "protocol/proto/protocol.proto"); err != nil {
			return in, err
		}
		ns := m.ClientExport.namespaces()
		if in.ids, err = protocol.ExportClientIDs(under(root, protocol.DefaultDefDir), ns.IDs); err != nil {
			return in, err
		}
		if in.sources.ProtocolDef, err = protocolDefHash(root); err != nil {
			return in, err
		}
	} else if in.ids, err = protocol.ExportClientIDs("", m.ClientExport.namespaces().IDs); err != nil {
		return in, err
	}
	if in.daoSyncProto, err = readOptional(root, "db/dao_sync.proto"); err != nil {
		return in, err
	}
	if in.daoSyncProto != nil {
		in.sources.DaoSync = contentHash(in.daoSyncProto)
		if in.daoSyncApply, err = readOptional(root, "db/dao_sync_apply.cs"); err != nil {
			return in, err
		}
	}
	if in.errcodes, err = codeerr.Definitions(root); err != nil {
		return in, fmt.Errorf("scan errcode definitions: %w", err)
	}
	return in, nil
}

// buildClientExport 算出导出目录的全部文件（键相对 <dir>）。csharp 需要 protoc，版本先核对。
func buildClientExport(root string, m Manifest) (map[string][]byte, error) {
	spec := m.ClientExport
	in, err := readClientExportInputs(root, m)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	ns := spec.namespaces()
	protoNamespace := func(namespace, fallback string) string {
		if namespace == fallback {
			return ""
		}
		return namespace
	}
	protos := map[string][]byte{}
	if in.protocolProto != nil {
		raw, err := withCSharpNamespace(in.protocolProto, protoNamespace(ns.Protocol, defaultProtocolNamespace))
		if err != nil {
			return nil, fmt.Errorf("protocol/proto/protocol.proto: %w", err)
		}
		files[exportProtocolProto] = raw
		protos["protocol.proto"] = raw
	}
	if in.daoSyncProto != nil {
		raw, err := withCSharpNamespace(in.daoSyncProto, protoNamespace(ns.DaoSync, defaultDaoSyncNamespace))
		if err != nil {
			return nil, fmt.Errorf("db/dao_sync.proto: %w", err)
		}
		files[exportDaoSyncProto] = raw
		protos["dao_sync.proto"] = raw
	}
	manifest := clientExportManifest{
		Kind:       clientExportKind,
		Schema:     clientExportSchema,
		Core:       projectCoreVersion(root),
		Generator:  codegenVersion(),
		Languages:  append([]string{}, spec.Languages...),
		Sources:    in.sources,
		Files:      map[string]string{},
		Messages:   in.ids.Messages,
		ErrorCodes: make([]clientErrorCode, 0, len(in.errcodes)),
	}
	for _, def := range in.errcodes {
		manifest.ErrorCodes = append(manifest.ErrorCodes, clientErrorCode{Code: def.Code, Name: def.Name, Message: def.Message})
	}
	if spec.csharp() {
		protoc, err := spec.resolveProtoc()
		if err != nil {
			return nil, err
		}
		manifest.Protoc = requiredProtocVersion()
		manifest.GoogleProtobuf = sdkGoogleProtobufVersion
		manifest.Namespaces = &ns
		generated, err := runProtocCSharp(protoc, protos)
		if err != nil {
			return nil, err
		}
		if raw, ok := generated["protocol.proto"]; ok {
			files[exportProtocolCS] = raw
		}
		if raw, ok := generated["dao_sync.proto"]; ok {
			files[exportDaoSyncCS] = raw
			if in.daoSyncApply == nil {
				return nil, errors.New("db/dao_sync.proto exists but db/dao_sync_apply.cs does not; run roost generate without client_export first or check the dao generator output")
			}
			apply, err := daoSyncApplyNamespace(in.daoSyncApply, ns.DaoSync)
			if err != nil {
				return nil, err
			}
			files[exportDaoSyncApply] = apply
		}
		files[exportMessageIDs] = in.ids.CSharp
		errorCodes, err := codeerr.GenerateCSharp(in.errcodes, ns.IDs)
		if err != nil {
			return nil, err
		}
		files[exportErrorCodes] = errorCodes
	}
	for name, raw := range files {
		manifest.Files[name] = contentHash(raw)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files[exportManifest] = append(raw, '\n')
	return files, nil
}

// generateClientExport 是 roost generate 的最后一步：整份写出导出目录。
func generateClientExport(root string, m Manifest, w io.Writer) error {
	files, err := buildClientExport(root, m)
	if err != nil {
		return err
	}
	dir := under(root, m.ClientExport.dir())
	for _, name := range clientExportFiles {
		target := filepath.Join(dir, filepath.FromSlash(name))
		raw, keep := files[name]
		if !keep {
			if err := os.Remove(target); err == nil {
				fmt.Fprintf(w, "removed: %s\n", target)
			} else if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("client export target is not a regular file: %s", target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		changed, err := writeFileIfChanged(target, raw)
		if err != nil {
			return err
		}
		printGenerated(w, target, changed)
	}
	return nil
}

func writeFileIfChanged(target string, raw []byte) (bool, error) {
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, raw) {
		return false, nil
	}
	return true, os.WriteFile(target, raw, 0o644)
}

func printGenerated(w io.Writer, target string, changed bool) {
	state := "unchanged"
	if changed {
		state = "generated"
	}
	fmt.Fprintf(w, "%s: %s\n", state, target)
}

// readClientExportManifest 读并认出导出清单。
func readClientExportManifest(file string) (clientExportManifest, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return clientExportManifest{}, err
	}
	var manifest clientExportManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return clientExportManifest{}, fmt.Errorf("%s: %w", file, err)
	}
	if manifest.Kind != clientExportKind {
		return clientExportManifest{}, fmt.Errorf("%s is not a roost client export manifest (kind %q)", file, manifest.Kind)
	}
	if manifest.Schema != clientExportSchema {
		return clientExportManifest{}, fmt.Errorf("%s has client export schema %d; this roost reads schema %d", file, manifest.Schema, clientExportSchema)
	}
	return manifest, nil
}

// verifyExportFiles 核对导出目录里每个登记文件的哈希：手改或拷了一半的目录在这里被拒。
func verifyExportFiles(dir string, manifest clientExportManifest) []string {
	var problems []string
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !validExportFileName(name) {
			problems = append(problems, fmt.Sprintf("manifest lists unexpected file %q", name))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s is missing", name))
			continue
		}
		if contentHash(raw) != manifest.Files[name] {
			problems = append(problems, fmt.Sprintf("%s does not match manifest.json (edited by hand?)", name))
		}
	}
	return problems
}

func validExportFileName(name string) bool {
	for _, known := range clientExportFiles {
		if name == known && name != exportManifest {
			return true
		}
	}
	return false
}

// checkClientExport 是 doctor 的 client-export:fresh：不跑 protoc，只核对导出目录与当前源头。
//   - manifest.json 存在、每个登记文件的哈希一致；
//   - 协议源头（protocol/def）与同步字段（db/def 现在生成的 dao_sync.proto）的哈希与 manifest 一致；
//   - 错误码（重新扫 errcode.Define）与 manifest 一致；
//   - languages 与 roost.yaml 一致。
//
// 生成物整体是否最新（含 protoc 输出与生成器版本）由 roost generate --check 负责。
func checkClientExport(root string, m Manifest) CheckItem {
	const name = "client-export:fresh"
	spec := m.ClientExport
	dir := under(root, spec.dir())
	fail := func(detail string) CheckItem {
		return CheckItem{Name: name, Status: StatusFail, Detail: detail + "; run roost generate and commit " + spec.dir()}
	}
	manifest, err := readClientExportManifest(filepath.Join(dir, exportManifest))
	if os.IsNotExist(err) {
		return fail(spec.dir() + "/manifest.json is missing")
	}
	if err != nil {
		return fail(err.Error())
	}
	problems := verifyExportFiles(dir, manifest)
	if strings.Join(manifest.Languages, ",") != strings.Join(spec.Languages, ",") {
		problems = append(problems, fmt.Sprintf("languages %v in manifest.json, %v in roost.yaml", manifest.Languages, spec.Languages))
	}
	var current clientExportSources
	if hasFeature(m, "protocol") {
		if current.ProtocolDef, err = protocolDefHash(root); err != nil {
			return fail("hash protocol/def: " + err.Error())
		}
	}
	if current.DaoSync, err = currentDaoSyncHash(root, m); err != nil {
		return fail("dao sync schema: " + err.Error())
	}
	if current.ProtocolDef != manifest.Sources.ProtocolDef {
		problems = append(problems, "protocol/def changed since the export was generated")
	}
	if current.DaoSync != manifest.Sources.DaoSync {
		problems = append(problems, "DAO sync fields (db/def) changed since the export was generated")
	}
	defs, err := codeerr.Definitions(root)
	if err != nil {
		return fail("scan errcode definitions: " + err.Error())
	}
	codes := make([]clientErrorCode, 0, len(defs))
	for _, def := range defs {
		codes = append(codes, clientErrorCode{Code: def.Code, Name: def.Name, Message: def.Message})
	}
	if !sameErrorCodes(codes, manifest.ErrorCodes) {
		problems = append(problems, "errcode definitions changed since the export was generated")
	}
	if len(problems) > 0 {
		return fail(spec.dir() + " is stale: " + strings.Join(problems, "; "))
	}
	return CheckItem{Name: name, Status: StatusOK, Detail: fmt.Sprintf("%s matches protocol/def, DAO sync fields and %d errcode(s)", spec.dir(), len(codes))}
}

func sameErrorCodes(a, b []clientErrorCode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// currentDaoSyncHash 是同步字段现在的哈希：工程开了 dao 时用 dao 生成器把 db/def 生成到临时目录取 dao_sync.proto，
// 不读工程里可能过期的 db/dao_sync.proto；没开 dao 时 generate 不会改它，直接取磁盘上的。
func currentDaoSyncHash(root string, m Manifest) (string, error) {
	if !hasFeature(m, "dao") {
		raw, err := readOptional(root, "db/dao_sync.proto")
		if err != nil || raw == nil {
			return "", err
		}
		return contentHash(raw), nil
	}
	defDir := under(root, dao.DefaultDefDir)
	if _, err := os.Stat(defDir); os.IsNotExist(err) {
		return "", nil
	}
	tmp, err := os.MkdirTemp("", "roost-client-export-dao-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := dao.Run([]string{"-def", defDir, "-out", tmp, "-pkg", "db"}, io.Discard); err != nil {
		return "", err
	}
	raw, err := readOptional(tmp, "dao_sync.proto")
	if err != nil || raw == nil {
		return "", err
	}
	return contentHash(raw), nil
}
