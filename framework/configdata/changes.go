package configdata

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// 两代快照之间的变化。
//
// 重载成功只说明“新数据通过了全部规则、已经发布”，不说明它和上一代有什么不同。运维最常见的误读是：
// 改了 configs/table 下的 CSV、忘了 `roost generate`，重载读的是 config_data.dir 下的 JSON，于是结果
// 照样是成功、版本号照样递增，价格却没变。Compare 按成员（表 / 对象 / 自定义数据）比较 finalize 时记下的
// 内容摘要——与 Snapshot.Hash 用的是同一份字节，所以“Hash 相同”与“没有成员变化”是同一个结论——并给出
// 表的行数，让重载结果能直接说出“shop_item 变了（25 行 → 26 行）”或“没有任何表变化”。

type memberKey struct {
	kind string
	name Name
}

type memberDigest struct {
	digest string
	// rows 是表的行数；对象与自定义数据为 -1。
	rows int
}

func newMemberDigest(kind string, payload []byte, value any) memberDigest {
	sum := sha256.Sum256(payload)
	digest := memberDigest{digest: hex.EncodeToString(sum[:]), rows: -1}
	if kind == "table" {
		if table, ok := value.(interface{ Len() int }); ok {
			digest.rows = table.Len()
		}
	}
	return digest
}

// MemberChange 是一个成员在两代之间的变化。Kind 为 "table"、"object" 或 "custom"。RowsBefore /
// RowsAfter 只对表有意义（新增的表 RowsBefore 为 0，删除的表 RowsAfter 为 0）；内容变了而行数相同
// 说明改的是行内字段。
type MemberChange struct {
	Kind       string `json:"kind"`
	Name       Name   `json:"name"`
	RowsBefore int    `json:"rows_before,omitempty"`
	RowsAfter  int    `json:"rows_after,omitempty"`
}

// Changes 是新一代相对上一代的逐成员变化。Initial 为 true 表示没有上一代（首次加载），此时全部成员
// 记在 Added 里。
type Changes struct {
	Initial   bool           `json:"initial,omitempty"`
	Added     []MemberChange `json:"added,omitempty"`
	Removed   []MemberChange `json:"removed,omitempty"`
	Changed   []MemberChange `json:"changed,omitempty"`
	Unchanged int            `json:"unchanged"`
}

// None 报告新一代与上一代内容完全相同（首次加载不算）。
func (c Changes) None() bool {
	return !c.Initial && len(c.Added) == 0 && len(c.Removed) == 0 && len(c.Changed) == 0
}

// ChangedNames 返回新增、删除与变化的成员名（按种类与名字排序），用于日志与简短回答。
func (c Changes) ChangedNames() []string {
	names := make([]string, 0, len(c.Added)+len(c.Removed)+len(c.Changed))
	for _, group := range [][]MemberChange{c.Added, c.Removed, c.Changed} {
		for _, change := range group {
			names = append(names, string(change.Name))
		}
	}
	sort.Strings(names)
	return names
}

func (c Changes) logAttrs() []any {
	return []any{"changed", len(c.Changed), "added", len(c.Added), "removed", len(c.Removed), "unchanged", c.Unchanged, "members", c.ChangedNames()}
}

// Compare 比较两代快照（old 为 nil 表示首次加载）。两者都必须是 Store 构建、已 finalize 的快照；
// 结果按种类、名字排序，顺序稳定。
func Compare(old, current *Snapshot) Changes {
	var changes Changes
	if current == nil {
		return changes
	}
	var before map[memberKey]memberDigest
	if old == nil {
		changes.Initial = true
	} else {
		before = old.members
	}
	after := current.members
	for key, now := range after {
		was, existed := before[key]
		change := MemberChange{Kind: key.kind, Name: key.name, RowsAfter: max(now.rows, 0)}
		switch {
		case !existed:
			changes.Added = append(changes.Added, change)
		case was.digest != now.digest:
			change.RowsBefore = max(was.rows, 0)
			changes.Changed = append(changes.Changed, change)
		default:
			changes.Unchanged++
		}
	}
	for key, was := range before {
		if _, kept := after[key]; !kept {
			changes.Removed = append(changes.Removed, MemberChange{Kind: key.kind, Name: key.name, RowsBefore: max(was.rows, 0)})
		}
	}
	for _, group := range []*[]MemberChange{&changes.Added, &changes.Removed, &changes.Changed} {
		sort.Slice(*group, func(i, j int) bool {
			a, b := (*group)[i], (*group)[j]
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			return a.Name < b.Name
		})
	}
	return changes
}
