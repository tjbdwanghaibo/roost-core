package account

import domain "github.com/tjbdwanghaibo/roost-core/service/account"

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// serverDirectory answers GetServer the way the account service would; calls counts the attempts.
type serverDirectory struct {
	domain.Accounts
	answer func() (domain.GameServer, error)
	calls  int
}

func (directory *serverDirectory) GetServer(_ context.Context, serverID int32) (domain.GameServer, error) {
	directory.calls++
	server, err := directory.answer()
	server.ID = serverID
	return server, err
}

func startGuard(t *testing.T, directory *serverDirectory) error {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", 1001)
	guard := NewServingGuardMod()
	guard.startupBudget, guard.retryInterval = 50*time.Millisecond, 10*time.Millisecond
	if err := guard.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(domain.CapabilityName, domain.Accounts(directory)); err != nil {
		t.Fatal(err)
	}
	if err := guard.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return guard.Start()
}

// 启动自检：本服已并入另一服就拒绝启动并点名承载服；没有合服、或本服还没登记，照常启动；account 一直查不到时
// 在预算内重试后拒绝启动（无法证明没被并入就不写数据）。
func TestTheServingGuardRefusesAMergedServer(t *testing.T) {
	merged := &serverDirectory{answer: func() (domain.GameServer, error) {
		return domain.GameServer{MergedInto: 1000, MergeNote: "merged in the 10-20 window"}, nil
	}}
	err := startGuard(t, merged)
	if !errors.Is(err, ErrServerMerged) || !strings.Contains(err.Error(), "merged into 1000") {
		t.Fatalf("a merged server started: %v", err)
	}
	for label, answer := range map[string]func() (domain.GameServer, error){
		"not merged": func() (domain.GameServer, error) { return domain.GameServer{Status: domain.ServerOpen}, nil },
		"not registered": func() (domain.GameServer, error) {
			return domain.GameServer{}, fmt.Errorf("%w: server 1001 is unknown", domain.ErrServerInvalid)
		},
	} {
		if err := startGuard(t, &serverDirectory{answer: answer}); err != nil {
			t.Fatalf("%s: the guard refused to start: %v", label, err)
		}
	}
	unreachable := &serverDirectory{answer: func() (domain.GameServer, error) { return domain.GameServer{}, context.DeadlineExceeded }}
	err = startGuard(t, unreachable)
	if err == nil || errors.Is(err, ErrServerMerged) || !errors.Is(err, context.DeadlineExceeded) || unreachable.calls < 2 {
		t.Fatalf("an unreachable account service = %v after %d attempts; want a refusal after retrying", err, unreachable.calls)
	}
	// 自检要早于 WAL 与 outbox 打开：DataEngine Mod 把它列为可选依赖，有它时排在它之后。
	if !slices.Contains(kitdataengine.NewMod().OptionalDependsOn(), mods.ModAccountServingGuard) {
		t.Fatal("the DataEngine Mod does not order itself after the serving guard")
	}
}
