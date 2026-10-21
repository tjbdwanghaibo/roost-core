package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// checkMergeReadiness 是 doctor 的 merge-readiness 一项（只读，OK / WARN，不让 doctor 失败）：工程带 cmd/sidmerge
// （game-demo 模板）时运行 `go run ./cmd/sidmerge checklist`，把合服清单（roost-core wiring/sidmerge.BuildChecklist，
// 与 sidmerge migrate -dry-run 同一份）概括成一行：哪些逻辑库按 sid 分库（合服时要搬）、哪些共享（两侧部署必须
// 同一位置）、哪些持久总线用法按 sid 寻址、多少项需要业务语义的合并。它不连任何依赖，所以不检查 WAL、outbox、saga
// 与冲突：那些只能在停服窗口里由 sidmerge migrate -dry-run 查（docs/framework/PLAYER-MIGRATION.md §6）。
//
// 生成器不能 import 运行时，清单经工程里的命令取得；这里只解码用到的字段。没有 cmd/sidmerge 的工程不报这一项。
func checkMergeReadiness(ctx context.Context, root string, goAvailable bool) (CheckItem, bool) {
	if _, err := os.Stat(filepath.Join(root, "cmd", "sidmerge", "main.go")); err != nil {
		return CheckItem{}, false
	}
	item := CheckItem{Name: "merge-readiness"}
	if !goAvailable {
		item.Status, item.Detail = StatusWarn, "go is not available, so the merge checklist (go run ./cmd/sidmerge checklist) was not read"
		return item, true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := runCommandTree(ctx, root, append(os.Environ(), "GOWORK=off"), &stdout, &stderr, "go", "run", "-buildvcs=false", "-mod=readonly", "./cmd/sidmerge", "checklist")
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail = "timed out after 3m"
		}
		if len(detail) > 1024 {
			detail = detail[len(detail)-1024:]
		}
		item.Status, item.Detail = StatusWarn, "go run ./cmd/sidmerge checklist failed: "+firstNonEmpty(detail, err.Error())
		return item, true
	}
	summary, err := summarizeMergeChecklist(stdout.Bytes())
	if err != nil {
		item.Status, item.Detail = StatusWarn, "read the merge checklist: "+err.Error()
		return item, true
	}
	item.Status, item.Detail = StatusOK, summary
	return item, true
}

// mergeChecklist 是 sidmerge.Checklist 里 doctor 用到的字段（JSON 名与 wiring/sidmerge 相同，
// TestDoctorMergeReadinessReadsTheChecklist 用真实的 game-demo 输出钉住）。
type mergeChecklist struct {
	SID       int32 `json:"sid"`
	Databases []struct {
		Logical      string `json:"logical"`
		ServerScoped bool   `json:"server_scoped"`
	} `json:"databases"`
	Bus []struct {
		Name           string `json:"name"`
		Persistent     bool   `json:"persistent"`
		AddressedToSID bool   `json:"addressed_to_sid"`
	} `json:"bus"`
	Business []struct {
		Name string `json:"name"`
	} `json:"business"`
}

func summarizeMergeChecklist(raw []byte) (string, error) {
	var list mergeChecklist
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", err
	}
	if len(list.Databases) == 0 {
		return "", errors.New("the checklist names no logical database; does cmd/sidmerge import the project's db package?")
	}
	var shared, scoped, addressed []string
	for _, database := range list.Databases {
		if database.ServerScoped {
			scoped = append(scoped, database.Logical)
		} else {
			shared = append(shared, database.Logical)
		}
	}
	for _, item := range list.Bus {
		if item.Persistent && item.AddressedToSID {
			addressed = append(addressed, item.Name)
		}
	}
	none := func(values []string) string {
		if len(values) == 0 {
			return "none"
		}
		return strings.Join(values, ", ")
	}
	return fmt.Sprintf("merging a server: dbscope=sid databases moved by sidmerge migrate: %s; shared databases both deployments must map to the same place: %s; "+
		"persistent bus items addressed to a sid (confirm by hand): %s; %d business merges to plan. "+
		"Full list: go run ./cmd/sidmerge checklist; runbook: docs/framework/PLAYER-MIGRATION.md §6",
		none(scoped), none(shared), none(addressed), len(list.Business)), nil
}
