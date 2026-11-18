package protocol

import (
	"bytes"
	"fmt"
	"os"
	"sort"
)

// DefaultCSharpIDsNamespace 是 -csharp 消息号表的命名空间；roost.yaml 的 client_export 没配 csharp.namespace 时
// 导出目录的 MessageIds.cs 也用它，与单独跑 -csharp 的输出逐字节相同。
const DefaultCSharpIDsNamespace = "Roost.Generated"

// ClientMessage 是客户端可见的一个消息号：请求与应答分开列（当前同号），推送单列。
// Name 是协议名（MessageIds 里去掉 Request / Response / Push 前缀的部分），Type 是 proto 消息名。
type ClientMessage struct {
	Name      string `json:"name"`
	Direction string `json:"direction"`
	ID        uint32 `json:"id"`
	Type      string `json:"type"`
}

// 客户端消息的方向：请求（客户端发）、应答（服务端回）、推送（服务端主动发）。
const (
	DirectionRequest  = "request"
	DirectionResponse = "response"
	DirectionPush     = "push"
)

// ClientIDs 是 roost generate 的 client_export 从 protocol/def 得到的客户端消息号：C# 与 TS 常量表、清单。
type ClientIDs struct {
	CSharp   []byte
	TS       []byte
	Messages []ClientMessage
}

// ExportClientIDs 读协议定义目录，产出 namespace 下的 C# 消息号表（与 -csharp 同一个生成函数）、TS 消息号表
// （message_ids.ts，名字与 C# 一一对应）和消息号清单。
// 目录不存在或没有协议时得到空表与空清单。清单按号、再按方向（请求、应答、推送）排序。
func ExportClientIDs(defDir, namespace string) (ClientIDs, error) {
	defs := &Definitions{}
	if _, err := os.Stat(defDir); err == nil {
		parsed, err := parseDefDir(defDir)
		if err != nil {
			return ClientIDs{}, fmt.Errorf("parse protocol defs: %w", err)
		}
		if len(parsed.Structs) > 0 {
			defs = parsed
		}
	} else if !os.IsNotExist(err) {
		return ClientIDs{}, err
	}
	raw, err := generateCSharpIDsIn(defs, namespace)
	if err != nil {
		return ClientIDs{}, err
	}
	messages := make([]ClientMessage, 0, 2*len(defs.Messages)+len(defs.Pushes))
	for _, message := range defs.Messages {
		messages = append(messages, ClientMessage{Name: msgName(message), Direction: DirectionRequest, ID: message.ReqID, Type: message.Req})
		if message.Resp != "" {
			messages = append(messages, ClientMessage{Name: msgName(message), Direction: DirectionResponse, ID: message.RespID, Type: message.Resp})
		}
	}
	for _, push := range defs.Pushes {
		messages = append(messages, ClientMessage{Name: pushName(push), Direction: DirectionPush, ID: push.MsgID, Type: push.Msg})
	}
	order := map[string]int{DirectionRequest: 0, DirectionResponse: 1, DirectionPush: 2}
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].ID != messages[j].ID {
			return messages[i].ID < messages[j].ID
		}
		return order[messages[i].Direction] < order[messages[j].Direction]
	})
	return ClientIDs{CSharp: raw, TS: generateTSIDs(defs), Messages: messages}, nil
}

// generateTSIDs 是 MessageIds.cs 的 TS 版（client_export 声明 ts 时写成 ts/message_ids.ts）：同一份 Definitions、同样的
// Request / Response / Push 名字，用法 MessageIds.RequestAddExp 与 C# 相同。名字由 Go 标识符转来，只含字母数字，作对象键
// 不会撞上 __proto__ 一类的特殊键。
func generateTSIDs(defs *Definitions) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n\n/** 客户端消息号（服务端 protocol/def）：请求、应答与推送。 */\nexport const MessageIds = {\n", protocolGeneratedHeader)
	for _, message := range defs.Messages {
		fmt.Fprintf(&b, "  Request%s: %d,\n", msgName(message), message.ReqID)
		if message.Resp != "" {
			fmt.Fprintf(&b, "  Response%s: %d,\n", msgName(message), message.RespID)
		}
	}
	for _, push := range defs.Pushes {
		fmt.Fprintf(&b, "  Push%s: %d,\n", pushName(push), push.MsgID)
	}
	b.WriteString("} as const;\n")
	return b.Bytes()
}
