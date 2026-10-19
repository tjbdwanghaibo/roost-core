package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const ManifestName = "roost.yaml"

// frameworkErrcodeLast 是 infra/base/errcode.FrameworkCodeLast 的副本：生成器不 import 运行时
// （根包 TestCoreDependencyBoundary），两处须同时修改。框架段 1–999 只由 errcode 包分配。
const frameworkErrcodeLast = 999

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var releaseVersionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// minimumVersions 是当前生成物要求的最低框架版本；latest 仍是默认策略。
// v1.28.0 生成物使用 DAO 持久文档（dataengine.NewStoredKeySet）、
// saga Engine.GetByBusinessKey 与原生步骤、wiring/selfsync、entitytimer 等 v1.28.0 才有的 API，不能接入更早的 core。
// 此版本尚未发布时，验收用本地 source-head replace；发布后再验证真正的 minimum 下载。
var minimumVersions = VersionSpec{
	Core: "v1.28.0",
}

type Manifest struct {
	Schema     int                    `yaml:"schema"`
	Project    ProjectSpec            `yaml:"project"`
	Versions   VersionSpec            `yaml:"versions"`
	CICD       CICDSpec               `yaml:"cicd"`
	SharedMods []string               `yaml:"shared_mods,omitempty"`
	Services   map[string]ServiceSpec `yaml:"services"`
	Access     map[string]AccessSpec  `yaml:"access,omitempty"`
	Features   []string               `yaml:"features,omitempty"`
	Sagas      []string               `yaml:"sagas,omitempty"`
	// Timers 是按实体的持久定时器（roost add timer --entity <Entity>，D11），键为实体名（snake）。
	Timers map[string]TimerSpec `yaml:"timers,omitempty"`
	IDs    map[string]IDSpace   `yaml:"ids,omitempty"`

	// configuredShutdown is each service's longest shutdown.total_timeout
	// found in the project's configs, filled by withConfiguredShutdown when
	// rendering into an existing project. The deployment grace periods never
	// go below it. Not part of roost.yaml.
	configuredShutdown map[string]time.Duration

	// servicePackages is the Go package name each internal/service/<name>/
	// directory already uses, filled by withServicePackages when rendering
	// into an existing project. Not part of roost.yaml.
	servicePackages map[string]string
}

type ProjectSpec struct {
	Name   string `yaml:"name"`
	Module string `yaml:"module"`
}

type VersionSpec struct {
	Core string `yaml:"core"`
}

// retiredVersionKeys 是 versions 下已退役、不再解码的键及其拒绝说明。严格解码本来就会拒绝未知键，
// 这里提前按名字拒绝，让错误说清为什么退役、该怎么改（手工删掉这一行），而不是只报 "field not found"。
//
// codegen：合仓后生成器就是 roost-core/codegen/cmd/roost，“codegen 版本”就是
// roost-core 版本，再写一个数字只会和 versions.core 不一致；生成的 Makefile 按 versions.core 运行生成器。
//
// skill / service：合仓前 roost-skill / roost-service 两个模块的版本策略；两者早已是 roost-core 的包，
// 不再有单独的版本。旧工程的 import 改写工具（upgrade --consolidate）一并删除。
//
// kit：合仓后 kit 是 roost-core 的包（wiring/），它的“版本”就是 versions.core；它过去的下限 v1.14.8
// 与基于它的 feature 检查（nettransport >= v1.1.0、saga >= v1.4.0）都已被 versions.core 的下限覆盖。
var retiredVersionKeys = map[string]string{
	"kit":     "kit is a package of roost-core (wiring/) and follows versions.core",
	"codegen": "the generator ships inside roost-core and the Makefile runs it at versions.core",
	"skill":   "skill is a package of roost-core and follows versions.core",
	"service": "the framework services are packages of roost-core and follow versions.core",
}

// rejectRetiredVersionKeys 在严格解码前检查 versions 下的退役键；raw 不是合法 YAML 时交给解码报错。
func rejectRetiredVersionKeys(raw []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil || len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var joined error
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "versions" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		versions := root.Content[i+1]
		for j := 0; j+1 < len(versions.Content); j += 2 {
			key := versions.Content[j].Value
			if reason, retired := retiredVersionKeys[key]; retired {
				joined = errors.Join(joined, fmt.Errorf("versions.%s (%s) is retired and no longer read: %s; delete the line by hand", key, versions.Content[j+1].Value, reason))
			}
		}
	}
	return joined
}

// CICDSpec contains only non-secret delivery policy. Credentials, production
// configuration and kubeconfig belong to protected deployment environments.
type CICDSpec struct {
	Provider     string   `yaml:"provider"`
	Registry     string   `yaml:"registry"`
	Environments []string `yaml:"environments"`
	Deploy       []string `yaml:"deploy"`
}

type ServiceSpec struct {
	Mods []string `yaml:"mods,omitempty"`
	// Framework names a roost-service service this process hosts — account,
	// mail, match or chat — instead of business code. The generated
	// bootstrap registers that service's Server with its owner Mod; the Mod's
	// collaborators come from internal/service/<name>/collaborators.go.
	Framework string `yaml:"framework,omitempty"`
	// Uses lists framework services (declared above) this business service
	// calls. Each adds the ClientMod to this process and a typed accessor in
	// internal/service/<name>/framework_clients_gen.go.
	Uses []string `yaml:"uses,omitempty"`
	// Rpcs lists the project's own cross-process services this business
	// service owns (roost add rpc): internal/rpc/<name>, whose owner Mod the
	// bootstrap assembles into this process.
	Rpcs []string `yaml:"rpcs,omitempty"`
	// UsesRpcs lists project rpcs (owned by another service) this business
	// service calls; each assembles the generated ClientMod here.
	UsesRpcs []string `yaml:"uses_rpcs,omitempty"`
}

// AccessSpec declares an application-owned request boundary. Access layers
// are deliberately separate from Kit Mods: they translate authenticated
// sessions and generated protocols into injected Nest senders.
type AccessSpec struct {
	Service    string   `yaml:"service"`
	Transports []string `yaml:"transports,omitempty"`
	// SelfSync（只用于 player）让每个在线玩家经 Entity Sync 收到自己的 Player 状态：生成的 bootstrap 改用
	// wiring/nest.NewModWithEntitySync 并装上 wiring/selfsync，player TCP Mod 接上连接生命周期，
	// protocol/def/self_sync.go 声明推送编号。Player 的标记缺省按 sync=true；没写 subjectPacker= 时 entity
	// 生成器写默认 packer（player_sync_packer_gen.go，"self" 视图含 DAO 全部 sync 字段），generate / doctor
	// 检查就绪情况。默认关。
	SelfSync bool `yaml:"self_sync,omitempty"`
}

// TimerSpec 是一个实体的持久定时器组件。project sync 按它重写生成部分（组件、节点类型、Tick handler、
// bootstrap 里的驱动装配）；业务回调文件只在缺失时创建。
type TimerSpec struct {
	// Component 是组件名（snake），缺省 timer：game/entities/<entity>/<component>_component_gen.go。
	Component string `yaml:"component"`
	// ID 是组件类型号（ids.component 段内），由 roost add timer 分配。
	ID int64 `yaml:"id"`
	// Service 是运行该实体 Nest 的服务：它的 bootstrap 装 wiring/entitytimer 驱动。
	Service string `yaml:"service"`
	// Dao 是存放节点的 DAO 类型（如 PlayerDao）：roost add timer 在它的定义里加节点 map 与种子两个字段。
	Dao string `yaml:"dao"`
}

type IDSpace struct {
	Min    int64              `yaml:"min,omitempty"`
	Max    int64              `yaml:"max,omitempty"`
	Groups map[string]IDRange `yaml:"groups,omitempty"`
}

type IDRange struct {
	Min int64 `yaml:"min"`
	Max int64 `yaml:"max"`
}

func DefaultManifest(name, module string, services, mods, features []string) Manifest {
	if module == "" {
		module = "github.com/tjbdwanghaibo/" + name
	}
	if len(services) == 0 {
		services = []string{"game"}
	}
	if len(features) == 0 {
		features = []string{"protocol", "config", "entity", "nest", "dao", "errcode"}
	}
	shared := []string{"lock", "ops", "statslog"}
	if len(mods) == 0 {
		mods = []string{"configdata", "etcd", "redis", "mongo", "nats", "syncbus", "remote_entity", "dataengine", "nest", "manager"}
	}
	svc := make(map[string]ServiceSpec, len(services))
	for _, service := range services {
		svc[service] = ServiceSpec{Mods: append([]string(nil), mods...)}
	}
	return Manifest{
		Schema:     1,
		Project:    ProjectSpec{Name: name, Module: module},
		Versions:   VersionSpec{Core: "latest"},
		CICD:       defaultCICDSpec(),
		SharedMods: shared,
		Services:   svc,
		Features:   uniqueSorted(features),
		IDs: map[string]IDSpace{
			"protocol":  {Groups: map[string]IDRange{"game": {Min: 10000, Max: 19999}}},
			"entity":    {Min: 1, Max: 255},
			"component": {Min: 2000, Max: 2999},
			"errcode":   {Min: 100000, Max: 199999},
		},
	}
}

func LoadManifest(root string) (Manifest, error) {
	manifest, err := decodeManifest(root)
	if err != nil {
		return Manifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// loadManifestForUpgrade decodes an older manifest without applying the
// current version floor first. The upgrade command merges the requested
// policies and validates the resulting manifest before it is saved. YAML
// fields remain strict so this path cannot silently discard unknown data.
func loadManifestForUpgrade(root string) (Manifest, error) {
	return decodeManifest(root)
}

func decodeManifest(root string) (Manifest, error) {
	path := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	if err := rejectRetiredVersionKeys(raw); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	manifest := Manifest{CICD: defaultCICDSpec()}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return manifest, nil
}

func (m Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf(`# Roost project manifest. Run make sync after editing.
# versions.* defaults to latest; go.mod records the concrete release resolved
# for one build. Minimum: core %s. The generator runs at versions.core.
`, minimumVersions.Core)
	return append([]byte(header), raw...), nil
}

func (m Manifest) Validate() error {
	var joined error
	if m.Schema != 1 {
		joined = errors.Join(joined, fmt.Errorf("unsupported schema %d", m.Schema))
	}
	if !validName(m.Project.Name) {
		joined = errors.Join(joined, fmt.Errorf("invalid project name %q", m.Project.Name))
	}
	if !validModulePath(m.Project.Module) {
		joined = errors.Join(joined, fmt.Errorf("invalid module %q", m.Project.Module))
	}
	if len(m.Services) == 0 {
		joined = errors.Join(joined, errors.New("at least one service is required"))
	}
	for name, service := range m.Services {
		if !validName(name) {
			joined = errors.Join(joined, fmt.Errorf("invalid service name %q", name))
		}
		if _, err := resolveMods(service.Mods); err != nil {
			joined = errors.Join(joined, fmt.Errorf("service %s: %w", name, err))
		}
		shared := make(map[string]bool, len(m.SharedMods))
		for _, mod := range m.SharedMods {
			shared[mod] = true
		}
		for _, mod := range service.Mods {
			if shared[mod] {
				joined = errors.Join(joined, fmt.Errorf("service %s repeats shared mod %q", name, mod))
			}
		}
	}
	if err := m.validateFrameworkServices(); err != nil {
		joined = errors.Join(joined, err)
	}
	for name, access := range m.Access {
		if name != "player" {
			joined = errors.Join(joined, fmt.Errorf("unsupported access layer %q; supported: player", name))
		}
		if !validName(access.Service) {
			joined = errors.Join(joined, fmt.Errorf("access.%s.service is invalid", name))
		} else if _, exists := m.Services[access.Service]; !exists {
			joined = errors.Join(joined, fmt.Errorf("access.%s references unknown service %q", name, access.Service))
		}
		seenTransports := make(map[string]bool, len(access.Transports))
		for _, transport := range access.Transports {
			if transport != "tcp" {
				joined = errors.Join(joined, fmt.Errorf("access.%s has unsupported transport %q; supported: tcp", name, transport))
			}
			if seenTransports[transport] {
				joined = errors.Join(joined, fmt.Errorf("access.%s repeats transport %q", name, transport))
			}
			seenTransports[transport] = true
		}
	}
	for name, access := range m.Access {
		if !access.SelfSync {
			continue
		}
		if name != "player" {
			joined = errors.Join(joined, fmt.Errorf("access.%s.self_sync is only supported on access.player", name))
		} else if !contains(access.Transports, "tcp") {
			joined = errors.Join(joined, errors.New("access.player.self_sync needs the tcp transport (the generated player TCP Mod carries the connection lifecycle); run: roost add transport tcp"))
		}
		if !contains(m.Features, "entity") {
			joined = errors.Join(joined, errors.New("access.player.self_sync needs the entity feature (the player Entity is what the client sees)"))
		}
	}
	if _, enabled := m.Access["player"]; enabled && !contains(m.Features, "protocol") {
		joined = errors.Join(joined, errors.New("access.player requires the protocol feature"))
	}
	if access, enabled := m.Access["player"]; enabled {
		if service, exists := m.Services[access.Service]; exists {
			resolved, err := resolveMods(service.Mods)
			if err == nil && !contains(resolved, "nest") {
				joined = errors.Join(joined, fmt.Errorf("access.player service %q requires the nest mod", access.Service))
			}
		}
	}
	if resolvedShared, err := resolveMods(m.SharedMods); err != nil {
		joined = errors.Join(joined, fmt.Errorf("shared_mods: %w", err))
	} else if contains(resolvedShared, "manager") {
		// Managers are selected per service type, so a process-wide manager
		// mod would start a service's singletons in every service. Putting it
		// in shared_mods is a wiring mistake, not a supported choice.
		joined = errors.Join(joined, errors.New("shared_mods: the manager mod is per service; declare it under services.<name>.mods"))
	}
	for _, feature := range m.Features {
		if !knownFeatures[feature] {
			joined = errors.Join(joined, fmt.Errorf("unknown feature %q", feature))
		}
	}
	if err := validateVersionPolicy("core", m.Versions.Core, minimumVersions.Core); err != nil {
		joined = errors.Join(joined, err)
	}
	if m.CICD.Provider != "github" {
		joined = errors.Join(joined, fmt.Errorf("cicd.provider must be github; got %q", m.CICD.Provider))
	}
	if m.CICD.Registry != "ghcr" {
		joined = errors.Join(joined, fmt.Errorf("cicd.registry must be ghcr; got %q", m.CICD.Registry))
	}
	for _, required := range []string{"staging", "production"} {
		if !contains(m.CICD.Environments, required) {
			joined = errors.Join(joined, fmt.Errorf("cicd.environments must contain %q", required))
		}
	}
	for _, required := range []string{"shell", "docker", "k8s"} {
		if !contains(m.CICD.Deploy, required) {
			joined = errors.Join(joined, fmt.Errorf("cicd.deploy must contain %q", required))
		}
	}
	for _, environment := range m.CICD.Environments {
		if !validName(environment) {
			joined = errors.Join(joined, fmt.Errorf("invalid cicd environment %q", environment))
		}
	}
	for _, deploy := range m.CICD.Deploy {
		if deploy != "shell" && deploy != "docker" && deploy != "k8s" {
			joined = errors.Join(joined, fmt.Errorf("unsupported cicd deploy target %q", deploy))
		}
	}
	for _, entityName := range sortedTimerEntities(m) {
		joined = errors.Join(joined, m.validateTimer(entityName))
	}
	for _, sagaName := range m.Sagas {
		if !validName(sagaName) {
			joined = errors.Join(joined, fmt.Errorf("invalid saga %q", sagaName))
		}
	}
	owners := map[string]string{}
	for _, name := range sortedServiceNames(m) {
		spec := m.Services[name]
		for _, rpc := range spec.Rpcs {
			if !validName(rpc) {
				joined = errors.Join(joined, fmt.Errorf("service %q: invalid rpc %q", name, rpc))
				continue
			}
			if m.isFrameworkService(name) {
				joined = errors.Join(joined, fmt.Errorf("service %q hosts a framework service and cannot own rpc %q", name, rpc))
			}
			if other, dup := owners[rpc]; dup {
				joined = errors.Join(joined, fmt.Errorf("rpc %q is owned by both %q and %q", rpc, other, name))
			}
			owners[rpc] = name
		}
	}
	for _, name := range sortedServiceNames(m) {
		spec := m.Services[name]
		for _, rpc := range spec.UsesRpcs {
			owner, ok := owners[rpc]
			if !ok {
				joined = errors.Join(joined, fmt.Errorf("service %q uses_rpcs %q, which no service owns (roost add rpc %s -service <owner>)", name, rpc, rpc))
			} else if owner == name {
				joined = errors.Join(joined, fmt.Errorf("service %q both owns and uses rpc %q; the owner reaches it in-process through the capability", name, rpc))
			}
			if m.isFrameworkService(name) {
				joined = errors.Join(joined, fmt.Errorf("service %q hosts a framework service and cannot declare uses_rpcs", name))
			}
		}
	}
	if len(owners) > 0 && !contains(m.Features, "rpc") {
		joined = errors.Join(joined, fmt.Errorf("services declare rpcs but the rpc feature is not enabled (roost add rpc enables it)"))
	}
	if len(m.Sagas) > 0 {
		hasSagaMod := false
		for _, service := range m.Services {
			hasSagaMod = hasSagaMod || contains(service.Mods, "saga")
		}
		if !hasSagaMod {
			joined = errors.Join(joined, errors.New("sagas require the saga mod on at least one service"))
		}
	}
	for kind, space := range m.IDs {
		if space.Min != 0 || space.Max != 0 {
			if space.Min <= 0 || space.Max < space.Min {
				joined = errors.Join(joined, fmt.Errorf("ids.%s has invalid range", kind))
			}
			if maximum, bounded := map[string]int64{"entity": 255, "component": 65535, "protocol": 4294967295}[kind]; bounded && space.Max > maximum {
				joined = errors.Join(joined, fmt.Errorf("ids.%s max %d exceeds framework encoding limit %d", kind, space.Max, maximum))
			}
			// 业务错误码不得落进框架段（errcode 包登记的码段归属）。
			if kind == "errcode" && space.Min <= frameworkErrcodeLast {
				joined = errors.Join(joined, fmt.Errorf("ids.errcode min %d overlaps the framework segment 1-%d; business codes start above it", space.Min, frameworkErrcodeLast))
			}
		}
		for group, r := range space.Groups {
			if !validName(group) || r.Min <= 0 || r.Max < r.Min {
				joined = errors.Join(joined, fmt.Errorf("ids.%s.groups.%s has invalid range", kind, group))
			}
			if kind == "protocol" && r.Max > 4294967295 {
				joined = errors.Join(joined, fmt.Errorf("ids.%s.groups.%s max %d exceeds framework encoding limit %d", kind, group, r.Max, uint64(4294967295)))
			}
		}
	}
	return joined
}

func defaultCICDSpec() CICDSpec {
	return CICDSpec{
		Provider:     "github",
		Registry:     "ghcr",
		Environments: []string{"staging", "production"},
		Deploy:       []string{"shell", "docker", "k8s"},
	}
}

func validName(name string) bool { return namePattern.MatchString(strings.TrimSpace(name)) }

func validModulePath(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 1024 || strings.Contains(value, "//") {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == '\\' || r == ':' || r == '"' || r == '\'' {
			return false
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".") {
			return false
		}
		for _, r := range segment {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)) {
				return false
			}
		}
	}
	return true
}

func versionAtLeast(version string, major, minor, patch int) bool {
	if strings.EqualFold(strings.TrimSpace(version), "latest") {
		return true
	}
	gotMajor, gotMinor, gotPatch, ok := releaseVersion(releasePrefix(version))
	if !ok {
		return false
	}
	if gotMajor != major {
		return gotMajor > major
	}
	if gotMinor != minor {
		return gotMinor > minor
	}
	return gotPatch >= patch
}

func validateVersionPolicy(name, value, minimum string) error {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "latest") {
		return nil
	}
	// A pre-release pin (v1.14.0-alpha.4) is judged by its release prefix: it
	// carries the layout of that release, which is what the floor protects.
	release := releasePrefix(value)
	major, _, _, ok := releaseVersion(release)
	if !ok {
		return fmt.Errorf("versions.%s must be latest or a semantic release such as %s; got %q", name, minimum, value)
	}
	minimumMajor, minimumMinor, minimumPatch, _ := releaseVersion(minimum)
	if major != minimumMajor {
		return fmt.Errorf("versions.%s major v%d is incompatible with the current module path; use latest or v%d.x", name, major, minimumMajor)
	}
	if !versionAtLeast(release, minimumMajor, minimumMinor, minimumPatch) {
		return fmt.Errorf("versions.%s requires >= %s or latest; got %q", name, minimum, value)
	}
	return nil
}

var preReleaseSuffix = regexp.MustCompile(`^-[0-9A-Za-z][0-9A-Za-z.-]*$`)

// releasePrefix strips a pre-release suffix: v1.14.0-alpha.4 carries the
// layout and API of v1.14.0, which is what every floor here protects.
func releasePrefix(version string) string {
	version = strings.TrimSpace(version)
	if i := strings.IndexByte(version, '-'); i > 0 && preReleaseSuffix.MatchString(version[i:]) {
		return version[:i]
	}
	return version
}

func releaseVersion(version string) (major, minor, patch int, ok bool) {
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return 0, 0, 0, false
	}
	if _, err := fmt.Sscanf(match[1]+"."+match[2]+"."+match[3], "%d.%d.%d", &major, &minor, &patch); err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

func resolvedModuleVersion(policy, minimum string) string {
	if strings.EqualFold(strings.TrimSpace(policy), "latest") {
		return minimum
	}
	return strings.TrimSpace(policy)
}

// splitList 解析逗号分隔的无序集合（Mod、feature）：去空、去重并排序，
// 使同一组值不论书写顺序都生成相同的 roost.yaml。顺序有含义的参数
// （Saga 步骤、-services 的第一个服务）用 splitOrderedList。
func splitList(raw string) []string {
	return uniqueSorted(splitOrderedList(raw))
}

// splitOrderedList 解析顺序有含义的逗号列表：只去掉空白项，保留书写顺序和
// 重复项，重复是否合法由使用方判断（Saga 步骤重复要报错，不能静默合并）。
func splitOrderedList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
