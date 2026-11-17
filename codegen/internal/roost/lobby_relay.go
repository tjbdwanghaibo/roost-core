package roost

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configfile"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"gopkg.in/yaml.v3"
)

// 大厅 lobby 与转发 relay（方案 docs/maintenance/FEATURE-2026-11-16-lobby-relay.md §4.5）。
//
// 两者都是 frameworkCatalog 里的 Direct 托管服务：services.<name>.framework: lobby / relay，直接面向客户端，没有总线客户端。
// 生成器为它们做的事：
//
//   - roost add lobby [name] / roost add relay [name]：声明托管服务（工程没有托管 account 时一并声明），sync 写出配置、
//     部署与 bootstrap，再补 account 配置的 account.services（工程里每个 lobby、relay 与玩家服务，kind 分别是
//     lobby / relay / game）与 account.games（lobby 服务的游戏）。lobby 进程装 account 的 ClientMod（经它校验游戏账号 token）。
//   - 配置段（directServiceConfigSection）：lobby.listen 用端口块的 8080+N，relay.listen / advertise_addr 用 7100+N、
//     WebSocket 用 7200+N；lobby.games 写一个以工程名命名的游戏；两边 token_key_env 都是 ROOST_RELAY_TOKEN_KEY，
//     subject、registry 前缀按工程名派生、两边相同。deploy/dev/run.sh 在变量没设时给一个开发用密钥。
//   - roost project new -template lobby：只有 account、lobby、relay 三个托管服务，不带 game（开房间场景不要 game，方案 §1）。
//   - doctor 的 lobby-relay:* 三项（checkLobbyRelay）。

// relayTokenKeyEnv 是生成配置里 lobby.token_key_env 与 relay.token_key_env 的值（两边必须是同一个变量）。
const relayTokenKeyEnv = "ROOST_RELAY_TOKEN_KEY"

// devRelayTokenKey 是 deploy/dev/run.sh 在 ROOST_RELAY_TOKEN_KEY 没设时用的开发密钥（至少 16 字节；生产必须另设）。
const devRelayTokenKey = "dev-relay-token-key-change-me"

// lobbyDefaultMaxMembers 与 accountDefaultGameTokenTTL 是 roost add lobby 写的游戏条目的缺省值。
const (
	lobbyDefaultMaxMembers     = 8
	accountDefaultGameTokenTTL = "2h"
)

// lobbyTemplateName 是 roost project new -template lobby。
const lobbyTemplateName = "lobby"

// hostedServiceOf 是托管 framework 的第一个服务名（按名字排序；没有时空串）。
func hostedServiceOf(m Manifest, framework string) string {
	for _, name := range sortedServiceNames(m) {
		if strings.TrimSpace(m.Services[name].Framework) == framework {
			return name
		}
	}
	return ""
}

// servicesHosting 是托管 framework 的全部服务名，按名字排序。
func servicesHosting(m Manifest, framework string) []string {
	var names []string
	for _, name := range sortedServiceNames(m) {
		if strings.TrimSpace(m.Services[name].Framework) == framework {
			names = append(names, name)
		}
	}
	return names
}

// hostsLobbyOrRelay 报告工程托管了 lobby 或 relay。
func hostsLobbyOrRelay(m Manifest) bool {
	return len(servicesHosting(m, "lobby")) > 0 || len(servicesHosting(m, "relay")) > 0
}

// lobbyGameName 是生成的 lobby.games / account.games 里的游戏名：工程名（游戏名的规则是 ^[a-z][a-z0-9_-]{0,31}$，
// 工程名满足前半，超长时截到 32 个字符）。
func lobbyGameName(project string) string {
	if len(project) > 32 {
		return project[:32]
	}
	return project
}

// requiredAccountServices 是 account.services 必须列出的服务：每个 lobby、relay 服务，以及玩家服务（access.player，kind game）。
// 只在工程托管了 lobby 或 relay 时要求：没有它们的工程不写 account.services，account 按单一 game 服务处理（行为不变）。
func requiredAccountServices(m Manifest) map[string]string {
	required := map[string]string{}
	if !hostsLobbyOrRelay(m) {
		return required
	}
	for _, kind := range []string{"lobby", "relay"} {
		for _, name := range servicesHosting(m, kind) {
			required[name] = kind
		}
	}
	if access, ok := m.Access["player"]; ok && strings.TrimSpace(access.Service) != "" {
		required[access.Service] = "game"
	}
	return required
}

// directServiceRequiredKeys 是 docs/SERVICES.zh-CN.md 表格里 lobby / relay 的必填配置。
func directServiceRequiredKeys(pkg string) string {
	switch pkg {
	case "lobby":
		return "lobby.key_prefix, lobby.games, lobby.token_key_env, lobby.relay_registry_prefix, lobby.subject"
	case "relay":
		return "relay.listen, relay.advertise_addr, relay.token_key_env, relay.subject, relay.registry_prefix"
	}
	return ""
}

// relayPorts 是 relay 服务在开发机上的 TCP 与 WebSocket 端口（端口块派生，见 port_blocks.go）。
func relayPorts(m Manifest, service string) (tcp, ws int) {
	block := portsOf(m, service).Block
	return 7100 + block, 7200 + block
}

// directServiceConfigSection 是 lobby / relay 服务配置里的托管服务段：kitConfigSchemas 的起始段，改写按端口块派生的
// 监听与对外地址，lobby 另写一个游戏条目（lobby.games 是 map，起始段写不出来；Init 要求至少一个）。
// relay.instance 不写：多个 sid 共用一份配置时，实例名取 <server_type>-<sid> 才不会撞。
func directServiceConfigSection(m Manifest, service string) string {
	framework := m.Services[service].Framework
	vars := projectNameVars(m.Project.Name)
	overrides := map[string]string{}
	drop := map[string]bool{}
	var extra []configschema.Key
	switch framework {
	case "lobby":
		overrides["lobby.listen"] = "0.0.0.0:" + strconv.Itoa(portsOf(m, service).Web)
		extra = append(extra, configschema.Key{
			Name: "lobby.games." + lobbyGameName(m.Project.Name) + ".max_members", Kind: configschema.KindInt,
			Starter: true, Example: strconv.Itoa(lobbyDefaultMaxMembers),
			Help: "本大厅服务的游戏（lobby.games.<游戏名>）与房间人数上限（含房主）；account.games 要有同名游戏（roost add lobby 写入）",
		})
	case "relay":
		tcp, ws := relayPorts(m, service)
		drop["relay.instance"] = true
		overrides["relay.listen"] = "0.0.0.0:" + strconv.Itoa(tcp)
		overrides["relay.advertise_addr"] = "127.0.0.1:" + strconv.Itoa(tcp)
		overrides["relay.ws_listen"] = "0.0.0.0:" + strconv.Itoa(ws)
		overrides["relay.ws_advertise_addr"] = "ws://127.0.0.1:" + strconv.Itoa(ws) + "/relay"
		// 开发时游戏页面开在本机的任意端口（vite 一类）；生产示例里 localhost 换成 CHANGE_ME，按游戏页面的域名改。
		overrides["relay.ws_allowed_origins"] = "localhost:*"
	}
	var schema configschema.Schema
	for _, key := range kitConfigSchemas[framework].Keys {
		if drop[key.Name] {
			continue
		}
		if value, ok := overrides[key.Name]; ok && key.Starter {
			key.Example = value
		}
		schema.Keys = append(schema.Keys, key)
		if key.Name == "lobby.games" {
			schema.Keys = append(schema.Keys, extra...)
		}
	}
	return schema.StarterYAML(vars)
}

// devRunRelayTokenKey 是 deploy/dev/run.sh 给托管 lobby / relay 的工程加的一段：变量没设时用开发密钥。
func devRunRelayTokenKey(m Manifest) string {
	if !hostsLobbyOrRelay(m) {
		return ""
	}
	return "# lobby / relay: the relay token HMAC key is read from this variable (lobby.token_key_env and\n" +
		"# relay.token_key_env name it); a fixed development key when it is unset. Production sets its own.\n" +
		relayTokenKeyEnv + "=\"${" + relayTokenKeyEnv + ":-" + devRelayTokenKey + "}\"\nexport " + relayTokenKeyEnv + "\n\n"
}

// directServicesGuide 是 docs/SERVICES.zh-CN.md 里 lobby / relay 的说明（工程托管了它们时才有）。
func directServicesGuide(m Manifest) string {
	if !hostsLobbyOrRelay(m) {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 大厅 lobby 与转发 relay\n\n")
	b.WriteString("lobby（HTTP）与 relay（TCP / WebSocket）直接面向客户端，不在总线上应答，业务服务不能 uses 它们。客户端的路径：\n")
	b.WriteString("account 登录拿账号票据 → 换游戏账号 token（portal 的 `POST /portal/game-token`）→ lobby 建房 / 加入，拿 relay 地址与令牌 → 连 relay。\n\n")
	b.WriteString("- lobby 进程装 account 的 ClientMod，经 `ValidateGameToken` 认游戏账号 token；lobby.games 的每个游戏在 account 配置的 account.games 里都要有。\n")
	b.WriteString("- account 配置的 account.services 列出本工程的 lobby、relay 与玩家服务（kind 分别是 lobby、relay、game），`roost project doctor` 的 lobby-relay:account-services 检查。\n")
	fmt.Fprintf(&b, "- relay 令牌的 HMAC 密钥放在环境变量 %s（lobby.token_key_env 与 relay.token_key_env 必须是同一个变量，doctor 检查；值至少 16 字节）。`make dev-run` 在它没设时用开发密钥，部署时自己设置。\n", relayTokenKeyEnv)
	b.WriteString("- relay.advertise_addr 是客户端连 relay 的地址，经 etcd 广播给 lobby；生产配置不能是回环地址（doctor 检查）。\n")
	b.WriteString("- 接口与契约见 roost-core 的 docs/framework/LOBBY.md 与 docs/framework/RELAY.md。部署模板不发布 lobby / relay 的客户端端口，按自己的负载均衡配置。\n\n")
	return b.String()
}

// addDirectService 是 roost add lobby / roost add relay：声明托管服务（没有托管 account 时一并声明 account），sync，
// 再补 account 配置（account.services、lobby 的 account.games）与 token_key_env（工程里已有的 lobby / relay 改过变量名时跟着它）。
// 已经是同名同种的托管服务时什么都不改（返回空列表）。
func addDirectService(root string, m Manifest, options AddOptions) ([]string, error) {
	kind := options.Kind
	name := toSnake(strings.TrimSpace(options.Name))
	if name == "" {
		name = kind
	}
	if !validName(name) {
		return nil, fmt.Errorf("invalid %s service name %q", kind, options.Name)
	}
	if existing, ok := m.Services[name]; ok {
		if strings.TrimSpace(existing.Framework) == kind {
			return nil, nil
		}
		return nil, fmt.Errorf("service %q already exists; name the %s service another way: roost add %s <name>", name, kind, kind)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	account := hostedServiceOf(m, "account")
	if account == "" {
		if _, taken := m.Services["account"]; taken {
			return nil, errors.New("the lobby and relay take their accounts from a hosted account service, and the name account is taken by a business service; host account under another name (services.<name>.framework: account) first")
		}
		account = "account"
		m.Services[account] = ServiceSpec{Framework: "account"}
	}
	m.Services[name] = ServiceSpec{Framework: kind}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("roost add %s: %w", kind, err)
	}
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, err
	}
	changed := []string{ManifestName,
		"configs/service/config." + name + ".yaml", "configs/service/config." + name + ".prod.example.yaml",
		"internal/bootstrap/generated.go", "deploy/dev/run.sh", "deploy/k8s/base/" + name + ".yaml", "docs/SERVICES.zh-CN.md"}
	if frameworkCatalog[kind].hasCollaborators() {
		changed = append(changed, m.serviceDir(name)+"/collaborators.go")
	}
	edited, err := alignTokenKeyEnv(root, m, name, options.Warnings)
	changed = append(changed, edited...)
	if err != nil {
		return uniqueOrdered(changed), err
	}
	edited, err = configureAccountForLobbyRelay(root, m, options.Warnings)
	changed = append(changed, edited...)
	return uniqueOrdered(changed), err
}

// serviceConfigRels 是一个服务的开发配置与生产示例（相对工程根）。
func serviceConfigRels(service string) []string {
	return []string{"configs/service/config." + service + ".yaml", "configs/service/config." + service + ".prod.example.yaml"}
}

// editServiceConfigs 对一个服务的开发配置、生产示例与 k8s Secret 示例施加同一个改写，返回改过的路径。文件不存在的跳过。
func editServiceConfigs(root, service string, warn io.Writer, edit func(text string) (string, error)) ([]string, error) {
	var changed []string
	for _, rel := range serviceConfigRels(service) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return changed, err
		}
		body, crlf := lfText(raw)
		updated, err := edit(body)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", rel, err)
		}
		if updated == body {
			continue
		}
		if err := writeAtomic(path, restoreLineEndings(updated, crlf), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, rel)
	}
	secret, err := editKubernetesSecretExampleConfig(root, service, warn, edit)
	if secret != "" {
		changed = append(changed, secret)
	}
	return changed, err
}

// configureAccountForLobbyRelay 给托管 account 的配置补上 account.services（requiredAccountServices）与 account.games
// （每个 lobby 开发配置里 lobby.games 的游戏），已有的条目不动。
func configureAccountForLobbyRelay(root string, m Manifest, warn io.Writer) ([]string, error) {
	account := hostedServiceOf(m, "account")
	if account == "" {
		return nil, nil
	}
	required := requiredAccountServices(m)
	services := make([]yamlMapEntry, 0, len(required))
	for _, name := range sortedMapKeys(required) {
		services = append(services, yamlMapEntry{Key: name, Lines: []string{"kind: " + required[name]}})
	}
	var games []yamlMapEntry
	for _, game := range lobbyGames(root, m) {
		games = append(games, yamlMapEntry{Key: game, Lines: []string{"token_ttl: " + accountDefaultGameTokenTTL}})
	}
	return editServiceConfigs(root, account, warn, func(text string) (string, error) {
		text, err := ensureYAMLMapEntries(text, "account", "services",
			"# 接受本账号体系的服务与类型（game | lobby | relay）；roost add lobby / relay 补上，doctor 的 lobby-relay:account-services 检查", services)
		if err != nil {
			return "", err
		}
		return ensureYAMLMapEntries(text, "account", "games",
			"# 签发游戏账号 token 的游戏（每个 lobby.games 的游戏都要有）与 token 寿命（至多 24h）", games)
	})
}

// lobbyGames 是工程里全部 lobby 服务开发配置 lobby.games 的游戏名，去重排序。
func lobbyGames(root string, m Manifest) []string {
	seen := map[string]bool{}
	for _, lobby := range servicesHosting(m, "lobby") {
		cfg, _, err := configfile.Load(filepath.Join(root, "configs", "service", "config."+lobby+".yaml"))
		if err != nil {
			continue
		}
		for game := range cfg.GetStringMap("lobby.games") {
			seen[game] = true
		}
	}
	return sortedMapKeys(seen)
}

// alignTokenKeyEnv：新加的 lobby / relay 服务的 token_key_env 跟着工程里已有的另一侧走（使用者改过变量名时）。
func alignTokenKeyEnv(root string, m Manifest, added string, warn io.Writer) ([]string, error) {
	addedKind := m.Services[added].Framework
	existing := ""
	for _, kind := range []string{"lobby", "relay"} {
		for _, name := range servicesHosting(m, kind) {
			if name == added || existing != "" {
				continue
			}
			if env := readConfigString(root, name, kind+".token_key_env"); env != "" {
				existing = env
			}
		}
	}
	if existing == "" || existing == relayTokenKeyEnv {
		return nil, nil
	}
	key := "token_key_env"
	return editServiceConfigs(root, added, warn, func(text string) (string, error) {
		document, err := parseYAMLDocument([]byte(text))
		if err != nil {
			return "", err
		}
		_, section := yamlMappingEntry(document, addedKind)
		keyNode, value := yamlMappingEntry(section, key)
		if keyNode == nil || value == nil || value.Value == existing {
			return text, nil
		}
		return replaceYAMLScalarLine(text, keyNode.Line, existing)
	})
}

// readConfigString 读一个服务开发配置里的字符串键；读不到时空串。
func readConfigString(root, service, key string) string {
	cfg, _, err := configfile.Load(filepath.Join(root, "configs", "service", "config."+service+".yaml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.GetString(key))
}

// yamlMapEntry 是 ensureYAMLMapEntries 要补的一个 map 条目：Key 下面的 Lines（相对条目的缩进）。
type yamlMapEntry struct {
	Key   string
	Lines []string
}

// ensureYAMLMapEntries 保证顶层 section 下的 map child 里有每个 entry（按 Key；已有的不动），缺哪一层补哪一层，
// 保留原文其余部分与注释。section、child 必须是块风格的 map（flow 风格报错，交给使用者手改）。
func ensureYAMLMapEntries(text, section, child, comment string, entries []yamlMapEntry) (string, error) {
	if len(entries) == 0 {
		return text, nil
	}
	document, err := parseYAMLDocument([]byte(text))
	if err != nil {
		return "", err
	}
	render := func(indent string, list []yamlMapEntry) []string {
		var lines []string
		for _, entry := range list {
			lines = append(lines, indent+entry.Key+":")
			for _, line := range entry.Lines {
				lines = append(lines, indent+"  "+line)
			}
		}
		return lines
	}
	sectionKey, sectionNode := yamlMappingEntry(document, section)
	if sectionNode == nil {
		block := append([]string{section + ":", "  " + comment, "  " + child + ":"}, render("    ", entries)...)
		return strings.TrimRight(text, "\n") + "\n" + strings.Join(block, "\n") + "\n", nil
	}
	if sectionNode.Kind != yaml.MappingNode || sectionNode.Style&yaml.FlowStyle != 0 {
		return "", fmt.Errorf("%s must use block-map YAML so codegen can add %s.%s", section, section, child)
	}
	indent := strings.Repeat(" ", sectionKey.Column+1)
	if len(sectionNode.Content) > 0 {
		indent = strings.Repeat(" ", sectionNode.Content[0].Column-1)
	}
	childKey, childNode := yamlMappingEntry(sectionNode, child)
	if childNode == nil || (childNode.Kind == yaml.ScalarNode && childNode.Tag == "!!null") {
		block := append([]string{indent + comment, indent + child + ":"}, render(indent+"  ", entries)...)
		if childKey != nil {
			// `child:` 写了但是空的：把空值那一行换成完整的块。
			lines := strings.Split(text, "\n")
			at := childKey.Line - 1
			lines = append(lines[:at], append(block[1:], lines[at+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
		return insertYAMLLines(text, sectionKey.Line, block)
	}
	if childNode.Kind != yaml.MappingNode || childNode.Style&yaml.FlowStyle != 0 {
		return "", fmt.Errorf("%s.%s must use block-map YAML so codegen can add entries to it", section, child)
	}
	var missing []yamlMapEntry
	for _, entry := range entries {
		if key, _ := yamlMappingEntry(childNode, entry.Key); key == nil {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return text, nil
	}
	entryIndent := strings.Repeat(" ", childKey.Column+1)
	if len(childNode.Content) > 0 {
		entryIndent = strings.Repeat(" ", childNode.Content[0].Column-1)
	}
	return insertYAMLLines(text, childKey.Line, render(entryIndent, missing))
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// applyLobbyTemplate 是 -template lobby 的清单：不要业务服务，只托管 account、lobby、relay（开房间场景，方案 §1 “不做 game”）。
func applyLobbyTemplate(m *Manifest, options NewOptions) error {
	if len(options.Services) > 0 && !(len(options.Services) == 1 && toSnake(options.Services[0]) == "game") {
		return errors.New("-template lobby has no business service; drop -services (it hosts account, lobby and relay)")
	}
	m.Services = map[string]ServiceSpec{
		"account": {Framework: "account"},
		"lobby":   {Framework: "lobby"},
		"relay":   {Framework: "relay"},
	}
	return nil
}

// ---- roost project doctor ----

// isLoopbackHost 报告 host:port（或裸 host）的主机是回环地址：localhost、127.0.0.0/8、::1。空主机（":7100"）不算：
// 那是“所有地址”的监听写法，不是对外地址，另由 relay 的 Init 拒绝。
func isLoopbackHost(addr string) bool {
	host := strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkLobbyRelay 是 doctor 的 lobby-relay:* 检查（工程托管了 lobby 或 relay 时才有）：
//
//   - lobby-relay:account-services：托管 account 的开发配置与生产示例里，account.services 列全了 requiredAccountServices
//     （每个 lobby、relay、玩家服务），kind 对得上；多出的条目（别的工程的服务）不管。
//   - lobby-relay:token-key-env：全部 lobby、relay 服务的开发配置与生产示例里 token_key_env 非空且是同一个变量
//     （lobby 签的 relay 令牌 relay 要用同一把密钥验）。
//   - lobby-relay:advertise-addr：每个 relay 的生产示例里 relay.advertise_addr 不是回环地址（客户端经 lobby 拿到它去连）。
func checkLobbyRelay(root string, m Manifest) []CheckItem {
	if !hostsLobbyOrRelay(m) {
		return nil
	}
	return []CheckItem{checkLobbyRelayAccountServices(root, m), checkLobbyRelayTokenKeyEnv(root, m), checkLobbyRelayAdvertise(root, m)}
}

// loadServiceConfig 读一个服务的配置文件（rel 相对工程根）；文件不存在时 found=false。
func loadServiceConfig(root, rel string) (cfg interface {
	GetString(string) string
	GetStringMap(string) map[string]any
}, found bool, err error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return nil, false, nil
	}
	loaded, _, err := configfile.Load(path)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", rel, err)
	}
	return loaded, true, nil
}

func checkLobbyRelayAccountServices(root string, m Manifest) CheckItem {
	const name = "lobby-relay:account-services"
	account := hostedServiceOf(m, "account")
	if account == "" {
		return CheckItem{Name: name, Status: StatusFail, Detail: "the lobby validates game account tokens through the account service and account.services lists the lobby and relay, but this project hosts no account service: roost add lobby declares it"}
	}
	required := requiredAccountServices(m)
	var problems []string
	for _, rel := range serviceConfigRels(account) {
		cfg, found, err := loadServiceConfig(root, rel)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !found {
			continue
		}
		listed := cfg.GetStringMap("account.services")
		for _, service := range sortedMapKeys(required) {
			entry, ok := listed[service]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: account.services has no %s (kind %s)", rel, service, required[service]))
				continue
			}
			kind := ""
			if fields, ok := entry.(map[string]any); ok {
				kind, _ = fields["kind"].(string)
			}
			if kind != required[service] {
				problems = append(problems, fmt.Sprintf("%s: account.services.%s.kind is %q, want %s", rel, service, kind, required[service]))
			}
		}
	}
	if len(problems) > 0 {
		return CheckItem{Name: name, Status: StatusFail, Detail: strings.Join(problems, "; ") + "; add them under account.services (roost add lobby / relay writes them)"}
	}
	return CheckItem{Name: name, Status: StatusOK, Detail: fmt.Sprintf("config.%s lists %s", account, strings.Join(sortedMapKeys(required), ", "))}
}

func checkLobbyRelayTokenKeyEnv(root string, m Manifest) CheckItem {
	const name = "lobby-relay:token-key-env"
	values := map[string][]string{}
	var problems []string
	for _, kind := range []string{"lobby", "relay"} {
		for _, service := range servicesHosting(m, kind) {
			for _, rel := range serviceConfigRels(service) {
				cfg, found, err := loadServiceConfig(root, rel)
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				if !found {
					continue
				}
				env := strings.TrimSpace(cfg.GetString(kind + ".token_key_env"))
				if env == "" {
					problems = append(problems, fmt.Sprintf("%s: %s.token_key_env is empty", rel, kind))
					continue
				}
				values[env] = append(values[env], rel)
			}
		}
	}
	if len(values) > 1 {
		var parts []string
		for _, env := range sortedMapKeys(values) {
			parts = append(parts, fmt.Sprintf("%s in %s", env, strings.Join(values[env], ", ")))
		}
		problems = append(problems, "lobby.token_key_env and relay.token_key_env name different variables ("+strings.Join(parts, "; ")+"): the relay would refuse every token the lobby signs")
	}
	if len(problems) > 0 {
		return CheckItem{Name: name, Status: StatusFail, Detail: strings.Join(problems, "; ")}
	}
	env := ""
	for key := range values {
		env = key
	}
	return CheckItem{Name: name, Status: StatusOK, Detail: "every lobby and relay reads the relay token key from " + env}
}

func checkLobbyRelayAdvertise(root string, m Manifest) CheckItem {
	const name = "lobby-relay:advertise-addr"
	relays := servicesHosting(m, "relay")
	if len(relays) == 0 {
		return CheckItem{Name: name, Status: StatusOK, Detail: "no relay service"}
	}
	var problems, checked []string
	for _, service := range relays {
		rel := "configs/service/config." + service + ".prod.example.yaml"
		cfg, found, err := loadServiceConfig(root, rel)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !found {
			continue
		}
		addr := strings.TrimSpace(cfg.GetString("relay.advertise_addr"))
		if addr == "" || isLoopbackHost(addr) {
			problems = append(problems, fmt.Sprintf("%s: relay.advertise_addr %q is empty or loopback; the lobby hands it to clients, which cannot reach a loopback address", rel, addr))
			continue
		}
		checked = append(checked, rel)
	}
	if len(problems) > 0 {
		return CheckItem{Name: name, Status: StatusFail, Detail: strings.Join(problems, "; ")}
	}
	if len(checked) == 0 {
		return CheckItem{Name: name, Status: StatusWarn, Detail: "no relay production example config to check (configs/service/config.<relay>.prod.example.yaml)"}
	}
	return CheckItem{Name: name, Status: StatusOK, Detail: "relay.advertise_addr is not loopback in " + strings.Join(checked, ", ")}
}

// printDirectServiceNext 是 roost add lobby / relay 之后的提示。
func printDirectServiceNext(stdout io.Writer, kind, name string, unchanged bool) {
	if unchanged {
		fmt.Fprintf(stdout, "%s is already hosted as service %s; nothing changed\n", kind, name)
		return
	}
	fmt.Fprintf(stdout, "next: set %s (at least 16 bytes; make dev-run uses a development key when it is unset), then make dev-run\n", relayTokenKeyEnv)
	switch kind {
	case "lobby":
		fmt.Fprintf(stdout, "next: configs/service/config.%s.yaml lobby.games lists the game this lobby serves; account.games needs the same game (roost add lobby wrote both)\n", name)
		fmt.Fprintln(stdout, "next: clients get a game account token from the account portal (roost module add portal: POST /portal/game-token), then call the lobby; see docs/SERVICES.zh-CN.md")
	case "relay":
		fmt.Fprintf(stdout, "next: in production set relay.advertise_addr in configs/service/config.%s.prod.example.yaml to the address clients reach (roost project doctor refuses loopback)\n", name)
	}
	fmt.Fprintln(stdout, "next: roost project doctor")
}
