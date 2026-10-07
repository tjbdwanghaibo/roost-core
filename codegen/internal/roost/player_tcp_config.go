package roost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
// access Mod reads (maintainer decision A4 ①). It is real Go here so the tags
// are checked when the generator starts (configschema.MustOf), and it is
// rendered into the generated server_gen.go as tcpConfig — so the generated
// Mod, the block `add transport tcp` writes, the reference copy and doctor all
// come from this one place. Bounds stay in the generated validateConfig, which
// also checks a Config built in code (NewServer); the defaults equal the
// generated defaultConfig() (the generated TestDeclaredDefaultsAreDefaultConfig).
//
// nest.request_timeout is kit/nest's key, read here as the fallback dispatch
// budget (RR-20260926-36); its declaration must equal kit/nest's
// (TestPlayerTCPDeclarationAgreesWithKit).
type playerTCPDeclaration struct {
	TCP struct {
		Enabled             bool          `config:"enabled" example:"false" help:"The listener stays off until auth.go is implemented and roost config enable player-tcp succeeds."`
		Addr                string        `config:"addr" default:"0.0.0.0:7000" example:"0.0.0.0:7000"`
		MaxConnections      int           `config:"max_connections" default:"10000" example:"10000"`
		MaxConnectionsPerIP int           `config:"max_connections_per_ip" default:"128" example:"128"`
		MaxHandshakes       int           `config:"max_handshakes" default:"1024" example:"1024"`
		MaxHandshakeBytes   uint32        `config:"max_handshake_bytes" default:"8192" example:"8192"`
		MaxPayloadBytes     uint32        `config:"max_payload_bytes" default:"1048576" example:"1048576"`
		HandshakeTimeout    time.Duration `config:"handshake_timeout" default:"5s" example:"5s"`
		IdleTimeout         time.Duration `config:"idle_timeout" default:"90s" example:"90s"`
		WriteTimeout        time.Duration `config:"write_timeout" default:"5s" example:"5s"`
		ShutdownTimeout     time.Duration `config:"shutdown_timeout" default:"10s" example:"10s" help:"The generated Mod declares it as its stop budget and the generated shutdown.total_timeout counts it (RR-20260927-05)."`
		DispatchTimeout     time.Duration `config:"dispatch_timeout" example:"3s" help:"Bound of one request; unset or 0 follows nest.request_timeout (RR-20260926-36)."`
		LoginTimeout        time.Duration `config:"login_timeout" example:"2s" help:"Share of the dispatch budget a login gives to claim + cold load; unset or 0 is min(2s, dispatch_timeout)."`
	} `config:"player_access.tcp"`
	Nest struct {
		RequestTimeout time.Duration `config:"request_timeout" min:"0" example:"3s"`
	} `config:"nest"`
}

var playerTCPSchemaOnce = sync.OnceValue(func() configschema.Schema { return configschema.MustOf(playerTCPDeclaration{}) })

// playerTCPSchema is the declaration of everything the generated Mod reads.
func playerTCPSchema() configschema.Schema { return playerTCPSchemaOnce() }

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
func renderPlayerTCPDeclaration() string {
	var b strings.Builder
	b.WriteString("// tcpConfig is what this Mod reads (maintainer decision A4 ①). Generated from the\n")
	b.WriteString("// generator's declaration; the App checks it before any Mod Init.\n")
	b.WriteString("type tcpConfig ")
	writeGoStructType(&b, reflect.TypeOf(playerTCPDeclaration{}), "")
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
// production example (RR-20260928-07).
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
			missing := make([]string, 0, len(defaults))
			for _, item := range defaults {
				if _, value := yamlMappingEntry(tcpNode, item.key); value == nil {
					missing = append(missing, "    "+item.key+": "+item.value)
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

// playerTCPDefaultsFor is playerTCPConfigDefaults for one service config: the
// dispatch budget is written equal to that config's nest.request_timeout when
// it has one, so a service whose Nest calls were given longer does not get a
// transport deadline that cuts them off (RR-20260926-36). The login budget
// stays at its default unless that would exceed the dispatch budget.
func playerTCPDefaultsFor(document *yaml.Node) []struct{ key, value string } {
	defaults := playerTCPConfigDefaults()
	_, nestNode := yamlMappingEntry(document, "nest")
	_, requestNode := yamlMappingEntry(nestNode, "request_timeout")
	if requestNode == nil || requestNode.Kind != yaml.ScalarNode {
		return defaults
	}
	request, err := time.ParseDuration(strings.TrimSpace(requestNode.Value))
	if err != nil || request <= 0 {
		return defaults
	}
	for index := range defaults {
		switch defaults[index].key {
		case "dispatch_timeout":
			defaults[index].value = request.String()
		case "login_timeout":
			if login, err := time.ParseDuration(defaults[index].value); err == nil && login > request {
				defaults[index].value = request.String()
			}
		}
	}
	return defaults
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
	for _, item := range defaults {
		builder.WriteString(valuePrefix + item.key + ": " + item.value + "\n")
	}
	return strings.TrimSuffix(builder.String(), "\n")
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
