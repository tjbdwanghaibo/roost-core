package roost

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 业务项目保管自己的上一发布产物；本命令既不从正在生成的目录猜基线，也不访问模块代理。
func runVerifyDAOSync(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("framework verify-dao-sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	previous := fs.String("previous", "", "dao_sync.proto from the last published client export")
	current := fs.String("current", "", "dao_sync.proto from the candidate export")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *previous == "" || *current == "" || fs.NArg() != 0 {
		return errors.New("usage: roost framework verify-dao-sync --previous <released dao_sync.proto> --current <candidate dao_sync.proto>")
	}
	protoc := os.Getenv("ROOST_PROTOC")
	if protoc == "" {
		protoc = "protoc"
	}
	binary, err := exec.LookPath(protoc)
	if err != nil {
		return fmt.Errorf("DAO schema verification requires protoc (set ROOST_PROTOC or PATH): %w", err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	old, err := readDAOSyncSchema(ctx, binary, *previous)
	if err != nil {
		return fmt.Errorf("previous %s: %w", *previous, err)
	}
	next, err := readDAOSyncSchema(ctx, binary, *current)
	if err != nil {
		return fmt.Errorf("current %s: %w", *current, err)
	}
	added, err := compareDAOSyncSchema(old, next)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "DAO sync schema compatible: %d appended field(s) on released messages, %d new message(s)\n", added, next.Messages().Len()-old.Messages().Len())
	return nil
}

// 用 protoc 而非逆向 Go 定义解析器：后者特意丢弃带 go_def 标记的生成消息与 reserved。
func readDAOSyncSchema(ctx context.Context, binary, path string) (protoreflect.FileDescriptor, error) {
	tmp, err := os.MkdirTemp("", "roost-dao-descriptor-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "schema.pb")
	cmd := exec.CommandContext(ctx, binary, "--proto_path=.", "--descriptor_set_out="+out, "./"+filepath.Base(path))
	cmd.Dir = filepath.Dir(path)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("protoc: %w: %s", err, strings.TrimSpace(string(msg)))
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, err
	}
	if len(set.File) != 1 || set.File[0].GetPackage() != "roost.dao.sync" || set.File[0].GetSyntax() != "proto3" || len(set.File[0].Dependency) != 0 {
		return nil, errors.New("expected a standalone proto3 roost.dao.sync schema")
	}
	fd, err := protodesc.NewFile(set.File[0], nil)
	if err != nil {
		return nil, err
	}
	root := false
	for i := 0; i < fd.Messages().Len(); i++ {
		m := fd.Messages().Get(i)
		root = root || daoSyncRoot(m)
		// DAO 嵌套业务类型也是文件顶层消息。唯一的词法嵌套是 protoc 创建的 map entry。
		for j := 0; j < m.Messages().Len(); j++ {
			if !m.Messages().Get(j).IsMapEntry() {
				return nil, fmt.Errorf("unsupported nested message: %s", m.Messages().Get(j).FullName())
			}
		}
		if m.Enums().Len() != 0 || m.Oneofs().Len() != 0 {
			return nil, fmt.Errorf("unsupported enum or oneof in %s", m.FullName())
		}
	}
	if !root || fd.Enums().Len() != 0 || fd.Services().Len() != 0 {
		return nil, errors.New("expected generated DAO sync messages including entity_id=1 and sync_format=2047; enums/services are unsupported")
	}
	return fd, nil
}

func daoSyncRoot(m protoreflect.MessageDescriptor) bool {
	id, format := m.Fields().ByNumber(1), m.Fields().ByNumber(2047)
	return id != nil && id.Name() == "entity_id" && id.Kind() == protoreflect.Int64Kind && !id.IsList() &&
		format != nil && format.Name() == "sync_format" && format.Kind() == protoreflect.Uint32Kind && !format.IsList()
}

// 字段按号、非文本顺序匹配；同 wire type 不代表同一语义，也不代表同一种值（例如 int32/sint32）。
func compareDAOSyncSchema(old, next protoreflect.FileDescriptor) (int, error) {
	var issues []string
	added := 0
	for i := 0; i < old.Messages().Len(); i++ {
		before := old.Messages().Get(i)
		after := next.Messages().ByName(before.Name())
		if after == nil {
			issues = append(issues, fmt.Sprintf("%s: released message removed", before.FullName()))
			continue
		}
		var last protoreflect.FieldNumber
		for j := 0; j < before.Fields().Len(); j++ {
			f := before.Fields().Get(j)
			// 2047 是固定元字段，不是业务字段的尾号；仍须像其他字段一样逐项比对。
			if !(daoSyncRoot(before) && f.Number() == 2047) && f.Number() > last {
				last = f.Number()
			}
			n := after.Fields().ByNumber(f.Number())
			prefix := fmt.Sprintf("%s #%d (%s)", before.FullName(), f.Number(), f.Name())
			switch {
			case n == nil:
				issues = append(issues, prefix+": released field removed or renumbered")
			case f.Name() != n.Name():
				issues = append(issues, fmt.Sprintf("%s: number now belongs to %s (rename or number reuse)", prefix, n.Name()))
			case daoFieldShape(f) != daoFieldShape(n):
				issues = append(issues, fmt.Sprintf("%s: %s changed to %s", prefix, daoFieldShape(f), daoFieldShape(n)))
			}
		}
		for j := 0; j < after.Fields().Len(); j++ {
			f := after.Fields().Get(j)
			if before.Fields().ByNumber(f.Number()) != nil {
				continue
			}
			if f.Number() <= last {
				issues = append(issues, fmt.Sprintf("%s #%d (%s): not an append after #%d", before.FullName(), f.Number(), f.Name(), last))
			} else {
				added++
			}
		}
		for j := 0; j < before.ReservedNames().Len(); j++ {
			if name := before.ReservedNames().Get(j); !after.ReservedNames().Has(name) {
				issues = append(issues, fmt.Sprintf("%s: reserved name %s no longer reserved", before.FullName(), name))
			}
		}
		for j := 0; j < before.ReservedRanges().Len(); j++ {
			r := before.ReservedRanges().Get(j)
			if !reservedRangeCovered(r, after.ReservedRanges()) {
				issues = append(issues, fmt.Sprintf("%s: reserved range %d..%d no longer fully reserved", before.FullName(), r[0], r[1]-1))
			}
		}
	}
	if len(issues) > 0 {
		sort.Strings(issues)
		return 0, fmt.Errorf("incompatible DAO sync schema:\n  %s", strings.Join(issues, "\n  "))
	}
	return added, nil
}

// 对区间而非区间内每个号码比较（reserved N to max 可以很大）。
func reservedRangeCovered(r [2]protoreflect.FieldNumber, ranges protoreflect.FieldRanges) bool {
	for p := r[0]; p < r[1]; {
		end := p
		for i := 0; i < ranges.Len(); i++ {
			n := ranges.Get(i)
			if n[0] <= p && p < n[1] {
				end = n[1]
				break
			}
		}
		if end == p {
			return false
		}
		p = end
	}
	return true
}

func daoFieldShape(f protoreflect.FieldDescriptor) string {
	if f.IsMap() {
		return fmt.Sprintf("map<%s,%s>", daoFieldShape(f.MapKey()), daoFieldShape(f.MapValue()))
	}
	typ := f.Kind().String()
	if f.Kind() == protoreflect.MessageKind {
		typ = string(f.Message().FullName())
	}
	if f.IsList() {
		return fmt.Sprintf("repeated %s (packed=%t)", typ, f.IsPacked())
	}
	return typ
}
