package roost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"

	"gopkg.in/yaml.v3"
)

// 接入层的配置声明同时驱动生成代码与 doctor，避免注册前门禁只在某一侧生效。
type playerAccessDeclaration struct {
	RequireWriteGate bool `config:"player_access.require_write_gate" default:"false" help:"要求写闸门；未注册期间拒绝请求，避免启动窗口放行"`
}

func playerAccessSchema() configschema.Schema { return configschema.MustOf(playerAccessDeclaration{}) }
func renderPlayerAccessDeclaration() string {
	var b strings.Builder
	b.WriteString("type accessConfig ")
	writeGoStructType(&b, reflect.TypeOf(playerAccessDeclaration{}), "")
	return b.String()
}

// playerTCPDeclaration is the declaration of what the generated player TCP
// access Mod reads (maintainer decision). It is real Go here so the tags
// are checked when the generator starts (configschema.MustOf), and it is
// rendered into the generated server_gen.go as tcpConfig — so the generated
// Mod, the block `add transport tcp` writes, the reference copy and doctor all
// come from this one place. Bounds stay in the generated validateConfig, which
// also checks a Config built in code (NewServer); the defaults equal the
// generated defaultConfig() (the generated TestDeclaredDefaultsAreDefaultConfig).
//
// nest.request_timeout is wiring/nest's key, read here as the fallback dispatch
// budget; its declaration must equal wiring/nest's
// (TestPlayerTCPDeclarationAgreesWithKit).
type playerTCPDeclaration struct {
	TCP struct {
		HeartbeatEnabled    bool          `config:"heartbeat_enabled" default:"true" example:"true" help:"鉴权后接受 RS v2 MsgID=0 空载荷心跳并回 ACK；客户端间隔应小于 idle_timeout。"`
		RequestRate         float64       `config:"request_rate" default:"100" example:"100" min:"0" max:"100000" help:"每连接每秒令牌数，包含心跳；0 显式关闭限流。"`
		RequestBurst        int           `config:"request_burst" default:"200" example:"200" min:"1" max:"100000" help:"每连接最大突发请求数，超出后断开。"`
		Enabled             bool          `config:"enabled" example:"false" help:"The listener stays off until auth.go is implemented and roost config enable player-tcp succeeds."`
		Addr                string        `config:"addr" listen:"player_access.tcp.enabled" default:"0.0.0.0:7000" example:"0.0.0.0:7000"`
		MaxConnections      int           `config:"max_connections" default:"10000" example:"10000"`
		MaxConnectionsPerIP int           `config:"max_connections_per_ip" default:"128" example:"128"`
		MaxHandshakes       int           `config:"max_handshakes" default:"1024" example:"1024"`
		MaxHandshakeBytes   uint32        `config:"max_handshake_bytes" default:"8192" example:"8192"`
		MaxPayloadBytes     uint32        `config:"max_payload_bytes" default:"1048576" example:"1048576"`
		HandshakeTimeout    time.Duration `config:"handshake_timeout" default:"5s" example:"5s"`
		IdleTimeout         time.Duration `config:"idle_timeout" default:"90s" example:"90s"`
		WriteTimeout        time.Duration `config:"write_timeout" default:"5s" example:"5s"`
		ShutdownTimeout     time.Duration `config:"shutdown_timeout" default:"10s" example:"10s" help:"The generated Mod declares it as its stop budget and the generated shutdown.total_timeout counts it."`
		DispatchTimeout     time.Duration `config:"dispatch_timeout" example:"0s" help:"Bound of one request; unset or 0 follows nest.request_timeout."`
		LoginTimeout        time.Duration `config:"login_timeout" example:"0s" help:"Share of the dispatch budget a login gives to claim + cold load; unset or 0 is min(2s, dispatch_timeout)."`
		// 全进程出站驻留预算，口径与独立 Gate 的 outbound.resident_bytes 相同；缺省理由见 gateway.defaultOutboundResidentBytes。
		OutboundResidentBytes int64 `config:"outbound.resident_bytes" default:"268435456" example:"268435456" min:"1" help:"全进程所有连接出站队列（排队 + 写协程在途）的字节合计上限，缺省 256 MiB；超出的推送被拒（ErrResidentBudgetExhausted，Sync 整轮重试），不断开连接。不小于 max_payload_bytes + 16。"`
	} `config:"player_access.tcp"`
	Nest struct {
		RequestTimeout time.Duration `config:"request_timeout" min:"0" example:"3s"`
	} `config:"nest"`
}

var playerTCPSchemaOnce = sync.OnceValue(func() configschema.Schema { return configschema.MustOf(playerTCPDeclaration{}) })

// playerTCPSchema is the declaration of everything the generated Mod reads
// when the player access has no websocket transport.
func playerTCPSchema() configschema.Schema { return playerTCPSchemaOnce() }

// playerWebSocketDeclaration 是 access.player.transports 含 websocket 时玩家接入另外读的键（GATEWAY.md
// “WebSocket 接入”）：嵌入 TCP 在 player_access.tcp.websocket 下，独立 Gate 在 player_access.gate.websocket 下。
// 生成器只把它们透传进 gateway.TCPConfig.WebSocket，规则（地址、路径、来源写法）由 gateway.ValidateTCPConfig 检查。
// 没有 websocket 传输时声明、生成代码与配置都与之前逐字节相同。addr 的 listen 标签按所在的段加（withWebSocketSection）：
// 嵌入 TCP 跟着 player_access.tcp.enabled，Gate 总是；空串表示不开，端口冲突检查不比较没写的地址（listenAddrProblems）。
type playerWebSocketDeclaration struct {
	Addr           string `config:"addr" help:"WebSocket 监听地址 host:port；空串不开。浏览器经它连接，每条二进制消息装一个 RS v2 帧，握手、票据、限额与 TCP 相同；TLS 由前置负载均衡终结（wss）"`
	Path           string `config:"path" help:"接受 WebSocket 升级的 HTTP 路径（精确匹配）；不写取 /ws"`
	AllowedOrigins string `config:"allowed_origins" help:"允许的浏览器页面来源，逗号分隔（game.example.com、https://*.cdn.example.com）；为空时拒绝所有带 Origin 头的浏览器请求（含同源），不带 Origin 的原生客户端总是放行"`
	TrustedProxies string `config:"trusted_proxies" help:"可信的前置七层负载均衡，逗号分隔的 CIDR 或单个地址（10.0.0.0/8, 192.168.1.10）；为空时不读 X-Forwarded-For，max_connections_per_ip 按 TCP 对端（负载均衡）计数。列出后，对端在列表里的连接按 X-Forwarded-For 从右往左第一个不可信的地址计数与鉴权；取不出时退回对端地址。0.0.0.0/0、::/0 被拒（等于让任何客户端伪造地址）"`
}

// renderPlayerWebSocketConfig 是把生成的配置声明里 websocket 段（selector，如 tcp.WebSocket）透传成
// gateway.WebSocketConfig 的表达式：Mod 读配置与 ValidateConfig 用同一份。
func renderPlayerWebSocketConfig(selector string) string {
	return fmt.Sprintf("gateway.WebSocketConfig{Addr: %[1]s.Addr, Path: %[1]s.Path, AllowedOrigins: %[1]s.AllowedOrigins, TrustedProxies: %[1]s.TrustedProxies}", selector)
}

// renderPlayerWebSocketValidate 是声明了 websocket 时生成的配置类型（typeName）的 ValidateConfig：按启动时的同一条规则
// （gateway.WebSocketConfig.Validate，规则本身在 wsconn）检查 websocket 段。App 在任何 Mod Init 之前、进程的
// --check-config、roost config check 与 doctor 的 config-check 都经声明的 Decode 调它，所以 trusted_proxies 写成
// 0.0.0.0/0 这类值在启动前就被拒，并点名本段的键。生成器自己不 import 运行时（dependency_boundary_test），只能这样把规则交给进程。
func renderPlayerWebSocketValidate(typeName, selector, section string) string {
	return fmt.Sprintf(`
// ValidateConfig 按启动时的同一条规则检查 websocket 段（gateway.WebSocketConfig.Validate）：App 启动前、--check-config、
// roost config check 与 doctor 的 config-check 都经它拒绝写错的值（例如 trusted_proxies 写成 0.0.0.0/0）。
func (c *%s) ValidateConfig(bool) error {
	return %s.Validate(%q)
}
`, typeName, renderPlayerWebSocketConfig("c."+selector), section)
}

// renderPlayerWebSocketGuide 是声明了 websocket 传输时接在 docs/PLAYER_ACCESS_TCP.zh-CN.md 后面的一节。
func renderPlayerWebSocketGuide(m Manifest) string {
	key := playerWebSocketAddrKey(m)
	section := strings.TrimSuffix(key, ".addr")
	return fmt.Sprintf(`
## WebSocket 接入

本工程声明了 websocket 传输（roost.yaml 的 access.player.transports）：浏览器经 WebSocket 连同一套玩家会话，每条二进制消息装一个 RS v2 帧，握手、票据、连接代号、路由、出站预算与 TCP 完全相同（roost-core 的 docs/framework/GATEWAY.md §6）。

- 配置在 %[1]s.{addr,path,allowed_origins,trusted_proxies}。生成器不写这些键：%[2]s 写上地址才开，path 不写取 /ws；allowed_origins 是逗号分隔的允许来源，为空时拒绝所有带 Origin 头的浏览器请求（含同源），不带 Origin 的原生客户端总是放行。
- 部署模板按约定的 %[3]d 发布这个端口（k8s 的 player-ws 容器与 Service 端口、生产 compose 的 ports、docker 示例的 -p）：开 WebSocket 时写 %[2]s: 0.0.0.0:%[3]d，换端口时同步改部署模板、负载均衡与防火墙。
- TLS（wss）由前置负载均衡终结。七层负载均衡后连接的对端是负载均衡：%[1]s.trusted_proxies 写上负载均衡自己的地址（逗号分隔的 CIDR 或单个地址，如 10.0.0.0/8），max_connections_per_ip 与鉴权拿到的地址就是 X-Forwarded-For 从右往左第一个不在列表里的地址；不写时不读这个头，全部 WebSocket 连接按负载均衡一个地址计数，要把上限调大。只认 X-Forwarded-For（Forwarded、X-Real-IP 不读），负载均衡要追加而不是原样转发客户端带来的这个头。0.0.0.0/0、::/0 启动与 roost config check / doctor 都拒绝：那等于让任何客户端伪造地址。对端可信却取不出地址的连接按负载均衡的地址计数，计 player_tcp_websocket_forwarded_fallback_total{reason}。
- 同一台机器上一起运行的服务，%[2]s 参与 doctor 的端口冲突检查（config-across-services）；没写地址时不参与。
- 没有声明 websocket 的工程写了 websocket 的键，roost config check 与 doctor 报失败：没有任何进程读它们。
`, section, key, playerWebSocketPort)
}

// playerWebSocketEnabled 报告 roost.yaml 的 access.player.transports 是否含 websocket。
func playerWebSocketEnabled(m Manifest) bool {
	access, ok := m.Access["player"]
	return ok && contains(access.Transports, "websocket")
}

// withWebSocketSection 返回 declaration 的一个无名副本：名为 section 的嵌套结构末尾多一个
// `WebSocket ... config:"websocket"` 字段。无名类型渲染进生成代码时是内联的结构体字面量，与原声明的写法一致。
// listen 是 websocket.addr 的 listen 标签（与同一段 TCP 地址的相同：嵌入 TCP 是 player_access.tcp.enabled，Gate 是 always），
// 让 WebSocket 地址参与同一台机器上的端口冲突检查。
func withWebSocketSection(declaration reflect.Type, section, listen string) reflect.Type {
	fields := make([]reflect.StructField, declaration.NumField())
	for i := range fields {
		fields[i] = declaration.Field(i)
		if fields[i].Name != section {
			continue
		}
		inner := make([]reflect.StructField, fields[i].Type.NumField())
		for j := range inner {
			inner[j] = fields[i].Type.Field(j)
		}
		webSocket := reflect.TypeOf(playerWebSocketDeclaration{})
		flat := make([]reflect.StructField, webSocket.NumField())
		for j := range flat {
			flat[j] = webSocket.Field(j)
			if flat[j].Name == "Addr" {
				flat[j].Tag = reflect.StructTag(strings.Replace(string(flat[j].Tag), `config:"addr"`, `config:"addr" listen:"`+listen+`"`, 1))
			}
		}
		fields[i].Type = reflect.StructOf(append(inner, reflect.StructField{
			Name: "WebSocket", Type: reflect.StructOf(flat), Tag: `config:"websocket"`,
		}))
	}
	return reflect.StructOf(fields)
}

// playerTCPDeclarationType 是生成的嵌入 TCP Mod 读的声明：有 websocket 传输时 TCP 段多一个 websocket 子段。
func playerTCPDeclarationType(m Manifest) reflect.Type {
	if !playerWebSocketEnabled(m) {
		return reflect.TypeOf(playerTCPDeclaration{})
	}
	return withWebSocketSection(reflect.TypeOf(playerTCPDeclaration{}), "TCP", "player_access.tcp.enabled")
}

// playerTCPSchemaFor 是 m 生成的嵌入 TCP Mod 读的全部键（doctor 的 config-schema 按它检查）。
func playerTCPSchemaFor(m Manifest) configschema.Schema {
	return configschema.MustOf(reflect.New(playerTCPDeclarationType(m)).Interface())
}

// playerTCPBlockSchema is the player_access.tcp part: what `add transport tcp`
// and the reference copy write.
func playerTCPBlockSchema() configschema.Schema {
	var block configschema.Schema
	for _, key := range playerTCPSchema().Keys {
		if strings.HasPrefix(key.Name, "player_access.tcp.") {
			block.Keys = append(block.Keys, key)
		}
	}
	return block
}

// playerTCPConfigDefaults is the player_access.tcp block as key / value pairs
// in declaration order (the starter values).
func playerTCPConfigDefaults() []struct{ key, value string } {
	var out []struct{ key, value string }
	for _, key := range playerTCPBlockSchema().Keys {
		if key.Starter {
			out = append(out, struct{ key, value string }{strings.TrimPrefix(key.Name, "player_access.tcp."), key.Example})
		}
	}
	return out
}

// renderPlayerTCPDeclaration renders playerTCPDeclaration as the generated
// tcpConfig type.
func renderPlayerTCPDeclaration(m Manifest) string {
	var b strings.Builder
	b.WriteString("// tcpConfig is what this Mod reads (maintainer decision). Generated from the\n")
	b.WriteString("// generator's declaration; the App checks it before any Mod Init.\n")
	b.WriteString("type tcpConfig ")
	writeGoStructType(&b, playerTCPDeclarationType(m), "")
	return b.String()
}

func writeGoStructType(b *strings.Builder, typ reflect.Type, indent string) {
	b.WriteString("struct {\n")
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		b.WriteString(indent + "\t" + field.Name + " ")
		if field.Type.Kind() == reflect.Struct && field.Type.Name() == "" {
			writeGoStructType(b, field.Type, indent+"\t")
		} else {
			b.WriteString(field.Type.String())
		}
		if field.Tag != "" {
			b.WriteString(" `" + string(field.Tag) + "`")
		}
		b.WriteString("\n")
	}
	b.WriteString(indent + "}")
}

// ensurePlayerTCPConfig adds only missing YAML keys and changes only the
// enabled scalar. It preserves the rest of the application-owned config byte
// for byte, including comments and ordering.
func ensurePlayerTCPConfig(root, service, explicitPath string, enabled bool) (string, error) {
	relative := explicitPath
	if relative == "" {
		relative = filepath.Join("configs", "service", "config."+service+".yaml")
	}
	path := relative
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.ToSlash(relative), err)
	}
	lineEnding := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	text, err := mergePlayerTCPConfig(strings.ReplaceAll(string(raw), "\r\n", "\n"), filepath.ToSlash(relative), enabled)
	if err != nil {
		return "", err
	}
	if lineEnding == "\r\n" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if err := writeAtomic(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	if filepath.IsAbs(relative) {
		if rel, relErr := filepath.Rel(root, relative); relErr == nil {
			relative = rel
		}
	}
	return filepath.Clean(relative), nil
}

// mergePlayerTCPConfig is ensurePlayerTCPConfig on one config text (LF line
// endings): missing player_access.tcp keys added, enabled set, the rest byte
// for byte. name is what errors call the text. `add transport tcp` applies it
// to the k8s Secret example's embedded config.yaml as well as to the
// production example.
func mergePlayerTCPConfig(text, name string, enabled bool) (string, error) {
	document, err := parseYAMLDocument([]byte(text))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	defaults := playerTCPDefaultsFor(document)
	playerKey, playerNode := yamlMappingEntry(document, "player_access")
	if playerNode == nil {
		text = strings.TrimRight(text, "\n") + "\n\n" + playerTCPBlock(0, defaults) + "\n"
	} else {
		if playerNode.Kind != yaml.MappingNode || playerNode.Style&yaml.FlowStyle != 0 {
			return "", errors.New("player_access must use block-map YAML so codegen can safely add tcp without rewriting application config")
		}
		tcpKey, tcpNode := yamlMappingEntry(playerNode, "tcp")
		if tcpNode == nil {
			text, err = insertYAMLLines(text, playerKey.Line, strings.Split(strings.TrimSuffix(playerTCPBlock(2, defaults), "\n"), "\n"))
			if err != nil {
				return "", err
			}
		} else {
			if tcpNode.Kind != yaml.MappingNode || tcpNode.Style&yaml.FlowStyle != 0 {
				return "", errors.New("player_access.tcp must use block-map YAML so codegen can safely fill defaults")
			}
			// 嵌套键（outbound.resident_bytes）：段已存在就补进段里（自下而上插，不挪动上方的行号），
			// 段不存在就连段名一起补在 tcp: 下。
			var missing []string
			var children []nestedInsert
			for _, group := range groupPlayerTCPDefaults(defaults) {
				if group.parent == "" {
					if _, value := yamlMappingEntry(tcpNode, group.items[0].key); value == nil {
						missing = append(missing, "    "+group.items[0].key+": "+group.items[0].value)
					}
					continue
				}
				parentKey, parentNode := yamlMappingEntry(tcpNode, group.parent)
				if parentNode == nil {
					missing = append(missing, "    "+group.parent+":")
					for _, item := range group.items {
						missing = append(missing, "      "+item.key+": "+item.value)
					}
					continue
				}
				if parentNode.Kind != yaml.MappingNode || parentNode.Style&yaml.FlowStyle != 0 {
					return "", fmt.Errorf("player_access.tcp.%s must use block-map YAML so codegen can safely fill defaults", group.parent)
				}
				indent := strings.Repeat(" ", parentKey.Column+1)
				if len(parentNode.Content) > 0 {
					indent = strings.Repeat(" ", parentNode.Content[0].Column-1)
				}
				for _, item := range group.items {
					if _, value := yamlMappingEntry(parentNode, item.key); value == nil {
						children = append(children, nestedInsert{line: parentKey.Line, text: indent + item.key + ": " + item.value})
					}
				}
			}
			slices.SortStableFunc(children, func(a, b nestedInsert) int { return b.line - a.line })
			for _, child := range children {
				if text, err = insertYAMLLines(text, child.line, []string{child.text}); err != nil {
					return "", err
				}
			}
			if len(missing) > 0 {
				text, err = insertYAMLLines(text, tcpKey.Line, missing)
				if err != nil {
					return "", err
				}
			}
		}
	}

	document, err = parseYAMLDocument([]byte(text))
	if err != nil {
		return "", fmt.Errorf("validate merged %s: %w", name, err)
	}
	_, playerNode = yamlMappingEntry(document, "player_access")
	_, tcpNode := yamlMappingEntry(playerNode, "tcp")
	_, enabledNode := yamlMappingEntry(tcpNode, "enabled")
	if enabledNode == nil {
		return "", errors.New("player_access.tcp.enabled was not created")
	}
	text, err = replaceYAMLScalarLine(text, enabledNode.Line, fmt.Sprint(enabled))
	if err != nil {
		return "", err
	}
	if _, err := parseYAMLDocument([]byte(text)); err != nil {
		return "", fmt.Errorf("validate updated %s: %w", name, err)
	}
	return text, nil
}

// playerTCPDefaultsFor 保留 0 的运行时继承语义，后续调整 Nest 预算无需同步改 TCP。
func playerTCPDefaultsFor(_ *yaml.Node) []struct{ key, value string } {
	return playerTCPConfigDefaults()
}

func playerTCPBlock(indent int, defaults []struct{ key, value string }) string {
	prefix := strings.Repeat(" ", indent)
	var builder strings.Builder
	valuePrefix := prefix + "  "
	if indent == 0 {
		builder.WriteString("player_access:\n  tcp:\n")
		valuePrefix = "    "
	} else {
		builder.WriteString(prefix + "tcp:\n")
	}
	for _, group := range groupPlayerTCPDefaults(defaults) {
		if group.parent == "" {
			builder.WriteString(valuePrefix + group.items[0].key + ": " + group.items[0].value + "\n")
			continue
		}
		builder.WriteString(valuePrefix + group.parent + ":\n")
		for _, item := range group.items {
			builder.WriteString(valuePrefix + "  " + item.key + ": " + item.value + "\n")
		}
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// playerTCPDefaultGroup 是 player_access.tcp 下的一个键（parent 为空）或一个段（如 outbound）的全部键，
// 段内的 key 去掉了段名。
type playerTCPDefaultGroup struct {
	parent string
	items  []struct{ key, value string }
}

// groupPlayerTCPDefaults 按声明顺序分组：同一段的键归到该段第一次出现的位置，只支持一层段。
func groupPlayerTCPDefaults(defaults []struct{ key, value string }) []playerTCPDefaultGroup {
	var groups []playerTCPDefaultGroup
	sections := map[string]int{}
	for _, item := range defaults {
		parent, child, nested := strings.Cut(item.key, ".")
		if !nested {
			groups = append(groups, playerTCPDefaultGroup{items: []struct{ key, value string }{item}})
			continue
		}
		index, ok := sections[parent]
		if !ok {
			index = len(groups)
			sections[parent] = index
			groups = append(groups, playerTCPDefaultGroup{parent: parent})
		}
		groups[index].items = append(groups[index].items, struct{ key, value string }{child, item.value})
	}
	return groups
}

// nestedInsert 是补进已有段里的一行：插在段名所在行 line 之后。
type nestedInsert struct {
	line int
	text string
}

func parseYAMLDocument(raw []byte) (*yaml.Node, error) {
	var root yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("service config root must be a YAML map")
	}
	return root.Content[0], nil
}

func yamlMappingEntry(mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index], mapping.Content[index+1]
		}
	}
	return nil, nil
}

func insertYAMLLines(text string, afterLine int, inserted []string) (string, error) {
	lines := strings.Split(text, "\n")
	if afterLine <= 0 || afterLine > len(lines) {
		return "", fmt.Errorf("invalid YAML insertion line %d", afterLine)
	}
	result := make([]string, 0, len(lines)+len(inserted))
	result = append(result, lines[:afterLine]...)
	result = append(result, inserted...)
	result = append(result, lines[afterLine:]...)
	return strings.Join(result, "\n"), nil
}

func replaceYAMLScalarLine(text string, lineNumber int, value string) (string, error) {
	lines := strings.Split(text, "\n")
	if lineNumber <= 0 || lineNumber > len(lines) {
		return "", fmt.Errorf("invalid YAML scalar line %d", lineNumber)
	}
	line := lines[lineNumber-1]
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return "", fmt.Errorf("YAML scalar line %d has no colon", lineNumber)
	}
	comment := ""
	if index := strings.Index(line[colon+1:], "#"); index >= 0 {
		comment = " " + strings.TrimSpace(line[colon+1+index:])
	}
	lines[lineNumber-1] = strings.TrimRight(line[:colon+1], " ") + " " + value + comment
	return strings.Join(lines, "\n"), nil
}
