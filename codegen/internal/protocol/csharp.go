package protocol

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 新增客户端输出必须先检查归属；不能用-force覆盖手写文件，目标文件不能是符号链接。
// 该检查在其他产物写入之前运行，拒绝时不留下半更新的协议集合。
func validateCSharpOutput(path string) error {
	if path == "" {
		return nil
	}
	if !strings.EqualFold(filepath.Ext(path), ".cs") {
		return fmt.Errorf("C# message id output must end in .cs: %s", path)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("C# output is not a regular file: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	firstLine, _, _ := bytes.Cut(raw, []byte("\n"))
	if !bytes.Equal(bytes.TrimSuffix(firstLine, []byte("\r")), []byte(protocolGeneratedHeader)) {
		return fmt.Errorf("refuse to replace application-owned C# output: %s", path)
	}
	return nil
}

// generateCSharpIDs 从同一份Definitions生成客户端路由编号；PB类型仍由对应proto生成。
func generateCSharpIDs(defs *Definitions) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\nnamespace Roost.Generated\n{\n    public static class MessageIds\n    {\n", protocolGeneratedHeader)
	for _, message := range defs.Messages {
		fmt.Fprintf(&b, "        public const uint Request%s = %d;\n", msgName(message), message.ReqID)
		if message.Resp != "" {
			fmt.Fprintf(&b, "        public const uint Response%s = %d;\n", msgName(message), message.RespID)
		}
	}
	for _, push := range defs.Pushes {
		fmt.Fprintf(&b, "        public const uint Push%s = %d;\n", pushName(push), push.MsgID)
	}
	fmt.Fprintf(&b, "    }\n}\n")
	return b.Bytes(), nil
}
