package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roost add effect（D12 方向 B）：载荷类型、topic 常量与 Emit 辅助在 game/effects，消费服务里一个 SubscribeEffects
// 骨架（毒消息 PoisonEffect）。都是业务文件；第二个消费服务只建自己的消费者，沿用已有的 effect 文件。
func TestAddEffectGeneratesPayloadEmitAndConsumer(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	if err := Run([]string{"add", "effect", "player.level_up", "-consumer", "game", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add effect: %v\n%s", err, out.String())
	}
	effect := readProjectFile(t, root, "game/effects/player_level_up.go")
	for _, want := range []string{
		"package effects",
		`const TopicPlayerLevelUp = "player.level_up"`,
		"type PlayerLevelUp struct {",
		"func EmitPlayerLevelUp(key string, event PlayerLevelUp) error",
		"nest.Emit(nest.Effect{Topic: TopicPlayerLevelUp, Key: key, Payload: payload})",
		// 组件里取玩家号要用 UniqueID：ID() 是编码后的 EntityID（新用户第七轮照直觉写 Owner().ID() 发出 9423877 而不是 9203）。
		"Owner().UniqueID()",
	} {
		if !strings.Contains(effect, want) {
			t.Errorf("game/effects/player_level_up.go lacks %q:\n%s", want, effect)
		}
	}
	consumer := readProjectFile(t, root, "internal/service/game/player_level_up_consumer.go")
	for _, want := range []string{
		"package game",
		`const PlayerLevelUpEffectDurable = "game-player-level_up"`,
		"func startPlayerLevelUpEffectConsumer(ctx context.Context, registry *app.Registry) (fnats.IJetStreamSubscription, error)",
		"kitdataengine.EffectConsumer{Durable: PlayerLevelUpEffectDurable, Topic: effects.TopicPlayerLevelUp}",
		"kitdataengine.SubscribeEffects(ctx, registry, consumer,",
		"return kitdataengine.PoisonEffect(",
		"envelope.EffectID",
		"has not been measured on its own",
	} {
		if !strings.Contains(consumer, want) {
			t.Errorf("player_level_up_consumer.go lacks %q:\n%s", want, consumer)
		}
	}
	for _, want := range []string{"effects.EmitPlayerLevelUp(key, effects.PlayerLevelUp{...})", "startPlayerLevelUpEffectConsumer(context.Background(), registry)", "subscription.Drain()"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("add effect next hints lack %q:\n%s", want, out.String())
		}
	}
	if service := readProjectFile(t, root, "internal/service/game/service.go"); strings.Contains(service, "EffectConsumer") {
		t.Errorf("add effect rewrote the service's Init:\n%s", service)
	}

	// 第二个消费服务：只建它的消费者，effect 文件（已被业务填了字段）不动。
	if err := Run([]string{"add", "service", "mailer", "-mods", "dataengine", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add service mailer: %v\n%s", err, out.String())
	}
	edited := strings.Replace(effect, "\t// TODO: the effect's fields", "\tPlayerID int64 `json:\"player_id\"`\n\t// TODO: the effect's fields", 1)
	writeProjectFile(t, root, "game/effects/player_level_up.go", edited)
	paths, err := Add(root, AddOptions{Kind: "effect", Name: "player.level_up", Consumer: "mailer"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "internal/service/mailer/player_level_up_consumer.go" {
		t.Errorf("second consumer wrote %v, want only its own consumer", paths)
	}
	if got := readProjectFile(t, root, "game/effects/player_level_up.go"); got != edited {
		t.Errorf("the second consumer rewrote the business's effect file:\n%s", got)
	}
	if mailer := readProjectFile(t, root, "internal/service/mailer/player_level_up_consumer.go"); !strings.Contains(mailer, `"mailer-player-level_up"`) {
		t.Errorf("mailer consumer durable:\n%s", mailer)
	}
}

// 错误输入在写盘之前被拒绝，工程逐字节不变：坏主题、saga.* 保留、未知服务、消费服务没有 dataengine、重复 add。
func TestAddEffectRejectsBadInputWithoutTouchingTheProject(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	if err := Run([]string{"add", "service", "lobby", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add service lobby: %v\n%s", err, out.String())
	}
	if _, err := Add(root, AddOptions{Kind: "effect", Name: "player.level_up", Consumer: "game"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "game", "effects", "hand_written.go"), []byte("package effects\n\nconst TopicHandWritten = \"hand.written\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := projectFileHashes(t, root)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"empty topic segment", []string{"player..level_up", "-consumer", "game"}, "invalid effect topic"},
		{"wildcard", []string{"player.*", "-consumer", "game"}, "invalid effect topic"},
		{"saga reserved", []string{"saga.result", "-consumer", "game"}, "saga.* carries the saga starts and completions"},
		{"unknown service", []string{"player.renamed", "-consumer", "nowhere"}, `unknown service "nowhere"`},
		{"ambiguous service", []string{"player.renamed"}, "effect requires -consumer <service> when the project has multiple services"},
		{"consumer without dataengine", []string{"player.renamed", "-consumer", "lobby"}, "effect consumer service lobby needs the dataengine Mod"},
		{"repeat", []string{"player.level_up", "-consumer", "game"}, "internal/service/game/player_level_up_consumer.go already exists"},
		{"hand-written effect file", []string{"hand.written", "-consumer", "game"}, `game/effects/hand_written.go exists but does not declare "type HandWritten struct"`},
		{"unsupported flag", []string{"player.renamed", "-consumer", "game", "-steps", "a"}, "unsupported flags for add effect: -steps"},
	} {
		err := Run(append([]string{"add", "effect"}, append(tc.args, "--root", root)...), &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
		assertProjectUnchanged(t, root, before)
	}
}
