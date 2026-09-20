package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const ManifestName = "roost.yaml"

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var releaseVersionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// minimumVersions is the oldest framework set supported by this generator.
// "latest" remains the default update policy; these concrete versions are
// used only as an offline/bootstrap go.mod baseline and as compatibility
// guards for users that intentionally pin a release.
//
// The consolidation (core v1.14.0 / kit v1.13.0) made the framework two
// modules: roost-skill lives in roost-core/skill and roost-service in
// roost-kit/service. A project pinning core below v1.14.0 must be upgraded
// with `roost project upgrade --consolidate`, which rewrites its imports.
//
// The floors below are HIGHER than that layout change, and they are not a
// guess: they are the oldest versions against which the generated code of the
// minimal and full templates actually compiles. What raised them is what the
// generators emit today — `entity.ValidateEntityRegistry`,
// `entity.EntityCategoryOther`, `platform.PendingOrders` — none of which
// exists at v1.14.0 / v1.13.0. The framework-compat workflow's `minimum` cell
// generates and compiles at exactly these versions, so a floor that stops
// being true fails there rather than in someone's first hour.
//
// The game-demo template needs more than this (it tracks the current release
// set) and is excluded from that cell on purpose.
var minimumVersions = VersionSpec{
	Core:    "v1.15.7",
	Kit:     "v1.14.8",
	Codegen: "v1.15.0",
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
	IDs        map[string]IDSpace     `yaml:"ids,omitempty"`
}

type ProjectSpec struct {
	Name   string `yaml:"name"`
	Module string `yaml:"module"`
}

type VersionSpec struct {
	Core string `yaml:"core"`
	Kit  string `yaml:"kit"`
	// Skill and Service are the pre-consolidation module policies. They are
	// still read so an old roost.yaml loads, but the modules no longer exist:
	// skill ships inside roost-core and the services inside roost-kit. Sync
	// drops the fields; the upgrader rewrites the project's imports.
	Skill   string `yaml:"skill,omitempty"`
	Service string `yaml:"service,omitempty"`
	Codegen string `yaml:"codegen"`
}

// consolidated reports whether the manifest still carries pre-consolidation
// module policies that sync must drop.
func (v VersionSpec) legacyModulePolicies() bool {
	return strings.TrimSpace(v.Skill) != "" || strings.TrimSpace(v.Service) != ""
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
		features = []string{"protocol", "config", "entity", "nest", "event", "dao", "errcode"}
	}
	shared := []string{"lock", "ops", "statslog"}
	if len(mods) == 0 {
		mods = []string{"configdata", "etcd", "redis", "mongo", "nats", "room", "remote_entity", "dataengine", "nest", "manager"}
	}
	svc := make(map[string]ServiceSpec, len(services))
	for _, service := range services {
		svc[service] = ServiceSpec{Mods: append([]string(nil), mods...)}
	}
	return Manifest{
		Schema:     1,
		Project:    ProjectSpec{Name: name, Module: module},
		Versions:   VersionSpec{Core: "latest", Kit: "latest", Skill: "latest", Service: "latest", Codegen: "latest"},
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
	manifest := Manifest{CICD: defaultCICDSpec()}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return manifest, nil
}

// legacyModNames and legacyFeatureNames map the pre-rename manifest vocabulary
// onto the current one. roost.yaml is hand-written and lives in user
// repositories, so an existing manifest must keep loading; the names are
// rewritten in memory and the canonical spelling is what `project sync` writes
// back out.
var legacyModNames = map[string]string{
	"sync": "room",
}

var legacyFeatureNames = map[string]string{
	"replication-quic": "nettransport-quic",
	"replication-kcp":  "nettransport-kcp",
	"replication-udp":  "nettransport-udp",
}

// canonicalizeLegacyNames rewrites legacy names in place. The receiver is a
// value, but mod and feature lists are slices and Services is a map, so the
// rewrite is visible to the caller — that is deliberate: Validate is the one
// gate every construction path passes through, so normalising here means a
// manifest is canonical by the time anything reads it.
func (m Manifest) canonicalizeLegacyNames() {
	rename := func(values []string, table map[string]string) {
		for i, value := range values {
			if canonical, ok := table[value]; ok {
				values[i] = canonical
			}
		}
	}
	rename(m.SharedMods, legacyModNames)
	for name, service := range m.Services {
		rename(service.Mods, legacyModNames)
		m.Services[name] = service
	}
	rename(m.Features, legacyFeatureNames)
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
# for one build. Minimums: core %s, kit %s, codegen %s.
`, minimumVersions.Core, minimumVersions.Kit, minimumVersions.Codegen)
	return append([]byte(header), raw...), nil
}

func (m Manifest) Validate() error {
	m.canonicalizeLegacyNames()
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
	for _, version := range []struct {
		name    string
		value   string
		minimum string
	}{
		{name: "core", value: m.Versions.Core, minimum: minimumVersions.Core},
		{name: "kit", value: m.Versions.Kit, minimum: minimumVersions.Kit},
		{name: "codegen", value: m.Versions.Codegen, minimum: minimumVersions.Codegen},
	} {
		if err := validateVersionPolicy(version.name, version.value, version.minimum); err != nil {
			joined = errors.Join(joined, err)
		}
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
	if hasNetTransportFeature(m) && !versionAtLeast(m.Versions.Kit, 1, 1, 0) {
		joined = errors.Join(joined, fmt.Errorf("replication features require roost-kit >= v1.1.0; got %q", m.Versions.Kit))
	}
	if contains(allProjectMods(m), "saga") && (!versionAtLeast(m.Versions.Core, 1, 4, 0) || !versionAtLeast(m.Versions.Kit, 1, 4, 0)) {
		joined = errors.Join(joined, fmt.Errorf("saga requires roost-core and roost-kit >= v1.4.0; got core=%q kit=%q", m.Versions.Core, m.Versions.Kit))
	}
	for kind, space := range m.IDs {
		if space.Min != 0 || space.Max != 0 {
			if space.Min <= 0 || space.Max < space.Min {
				joined = errors.Join(joined, fmt.Errorf("ids.%s has invalid range", kind))
			}
			if maximum, bounded := map[string]int64{"entity": 255, "component": 65535, "protocol": 4294967295}[kind]; bounded && space.Max > maximum {
				joined = errors.Join(joined, fmt.Errorf("ids.%s max %d exceeds framework encoding limit %d", kind, space.Max, maximum))
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

func hasNetTransportFeature(m Manifest) bool {
	return contains(m.Features, "nettransport-quic") || contains(m.Features, "nettransport-kcp") || contains(m.Features, "nettransport-udp")
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

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return uniqueSorted(out)
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
