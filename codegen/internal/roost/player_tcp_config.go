package roost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var playerTCPConfigDefaults = []struct{ key, value string }{
	{key: "enabled", value: "false"},
	{key: "addr", value: "0.0.0.0:7000"},
	{key: "max_connections", value: "10000"},
	{key: "max_connections_per_ip", value: "128"},
	{key: "max_handshakes", value: "1024"},
	{key: "max_handshake_bytes", value: "8192"},
	{key: "max_payload_bytes", value: "1048576"},
	{key: "handshake_timeout", value: "5s"},
	{key: "idle_timeout", value: "90s"},
	{key: "write_timeout", value: "5s"},
	{key: "shutdown_timeout", value: "10s"},
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
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	document, err := parseYAMLDocument([]byte(text))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", filepath.ToSlash(relative), err)
	}
	playerKey, playerNode := yamlMappingEntry(document, "player_access")
	if playerNode == nil {
		text = strings.TrimRight(text, "\n") + "\n\n" + playerTCPBlock(0) + "\n"
	} else {
		if playerNode.Kind != yaml.MappingNode || playerNode.Style&yaml.FlowStyle != 0 {
			return "", errors.New("player_access must use block-map YAML so codegen can safely add tcp without rewriting application config")
		}
		tcpKey, tcpNode := yamlMappingEntry(playerNode, "tcp")
		if tcpNode == nil {
			text, err = insertYAMLLines(text, playerKey.Line, strings.Split(strings.TrimSuffix(playerTCPBlock(2), "\n"), "\n"))
			if err != nil {
				return "", err
			}
		} else {
			if tcpNode.Kind != yaml.MappingNode || tcpNode.Style&yaml.FlowStyle != 0 {
				return "", errors.New("player_access.tcp must use block-map YAML so codegen can safely fill defaults")
			}
			missing := make([]string, 0, len(playerTCPConfigDefaults))
			for _, item := range playerTCPConfigDefaults {
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
		return "", fmt.Errorf("validate merged %s: %w", filepath.ToSlash(relative), err)
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
		return "", fmt.Errorf("validate updated %s: %w", filepath.ToSlash(relative), err)
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

func playerTCPBlock(indent int) string {
	prefix := strings.Repeat(" ", indent)
	var builder strings.Builder
	valuePrefix := prefix + "  "
	if indent == 0 {
		builder.WriteString("player_access:\n  tcp:\n")
		valuePrefix = "    "
	} else {
		builder.WriteString(prefix + "tcp:\n")
	}
	for _, item := range playerTCPConfigDefaults {
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
