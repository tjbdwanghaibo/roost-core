package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	gamelevel "github.com/tjbdwanghaibo/roost-core/gameplay/level"
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

type capturedRooms struct {
	domain.Rooms
	spec domain.Spec
}

func (*capturedRooms) Describe(context.Context) (string, error) { return "owner-1", nil }
func (r *capturedRooms) Create(_ context.Context, spec domain.Spec) (domain.Ref, error) {
	r.spec = spec
	return domain.Ref{ID: spec.ID, Epoch: 1}, nil
}

// 开房时把关卡的玩家、区域、出生点与流程一起交给房间；缺省的 CrowdScene 没有流程，这些字段为空。
func TestSeatCreatesTheRoomWithTheLevelFlow(t *testing.T) {
	ctx := context.Background()
	level := gamephysics.ObstacleLevel()
	data, _, err := level.Content()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "obstacles.json")
	if err = os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSeatsWith(path)
	if err = s.load(); err != nil {
		t.Fatal(err)
	}
	rooms := &capturedRooms{}
	s.rooms = rooms
	ref, body, err := s.seat(ctx, gateway.Binding{PlayerID: 2})
	if err != nil || body != 2 || ref.ID == "" {
		t.Fatalf("seat: %+v %d %v", ref, body, err)
	}
	got := rooms.spec
	if got.Instance != "owner-1" || !reflect.DeepEqual(got.Players, level.Players) || !reflect.DeepEqual(got.Regions, level.Regions) ||
		!reflect.DeepEqual(got.Spawns, level.Spawns) || got.DefaultSpawn != "start" || len(got.Flow) == 0 {
		t.Fatalf("room spec lost level fields: players=%v regions=%d spawns=%d default=%q flow=%d bytes", got.Players, len(got.Regions), len(got.Spawns), got.DefaultSpawn, len(got.Flow))
	}
	// 交给房间的流程就是关卡里那一份：能和这些区域、出生点一起编译。
	if _, err = gamephysics.CompileFlow(got.Flow, got.Regions, got.Spawns, gamelevel.Registry{}); err != nil {
		t.Fatalf("room flow does not compile: %v", err)
	}

	crowd := newSeats()
	if err = crowd.load(); err != nil {
		t.Fatal(err)
	}
	plain := &capturedRooms{}
	crowd.rooms = plain
	if _, _, err = crowd.seat(ctx, gateway.Binding{PlayerID: 1}); err != nil {
		t.Fatal(err)
	}
	if len(plain.spec.Flow) != 0 || len(plain.spec.Regions) != 0 || len(plain.spec.Spawns) != 0 || plain.spec.DefaultSpawn != "" {
		t.Fatalf("crowd room got flow fields: %+v", plain.spec)
	}
	if levelMessageID == frameMessageID || levelMessageID == inputMessageID || levelMessageID == contentMessageID || levelMessageID != 0x50485904 {
		t.Fatalf("level message id %#x", levelMessageID)
	}
}
