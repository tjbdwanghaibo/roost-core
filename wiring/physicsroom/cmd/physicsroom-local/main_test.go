package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// 关卡在 game 服务 Init 前不可用；缺省 CrowdScene 与 --level 文件都按槽号给出与 Seat 一致的受控刚体。
func TestSeatsContentFollowsTheLoadedLevel(t *testing.T) {
	ctx := context.Background()
	s := newSeats()
	if _, err := s.content(ctx, gateway.Binding{PlayerID: 1}); !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("content before load: %v", err)
	}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	c, err := s.content(ctx, gateway.Binding{PlayerID: 13})
	if err != nil || c.Body != uint64(gamephysics.CrowdCapsules+13) || c.SHA256 == "" {
		t.Fatalf("crowd content body=%d err=%v", c.Body, err)
	}
	if _, err = s.content(ctx, gateway.Binding{PlayerID: gamephysics.CrowdPlayers + 1}); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("slot beyond crowd: %v", err)
	}

	data, _, err := gamephysics.ObstacleLevel().Content()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "obstacles.json")
	if err = os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s = newSeats()
	s.levelPath = path
	if err = s.load(); err != nil {
		t.Fatal(err)
	}
	if c, err = s.content(ctx, gateway.Binding{PlayerID: 2}); err != nil || c.Body != 2 || string(c.Data) != string(data) {
		t.Fatalf("level content body=%d err=%v", c.Body, err)
	}
	if _, err = s.content(ctx, gateway.Binding{PlayerID: 3}); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("slot beyond level: %v", err)
	}
	if err = os.WriteFile(path, []byte(`{"Version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = newSeatsWith(path).load(); err == nil {
		t.Fatal("invalid level accepted")
	}
}

func newSeatsWith(path string) *seats { s := newSeats(); s.levelPath = path; return s }
