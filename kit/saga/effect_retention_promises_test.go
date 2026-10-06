package saga

import (
	"strings"
	"testing"
)

// O-S5-2（维护者第十二轮决定）：原生 Nest 步骤的完成结果走 DataEngine 的效果流，那条流的保留期是
// dataengine.effects.max_age。完成回执（saga.completion_receipt_ttl）必须比它活得久：回执过期之后
// 流里还留着的结果再投递一次，协调器手里既没有回执也没有 tombstone，分不清“已计入的重复”和“放弃
// 之后才生效的成功”，只能 ErrNotWaiting → Term（REVIEW-2026-10-06-n06s5 §4）。saga 流本身的前提
// （ttl > saga.stream_max_age）启动时早就校验；效果流跨 Mod，之前没有校验，违反时只有一条通用的
// terminal 计数。现在 saga Mod 在结果效果流就是 DataEngine 效果流时一并校验，不满足拒绝启动并点名
// 两个键。
func TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts(t *testing.T) {
	cfg := yamlConfig(t, "dataengine:\n  effects:\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n")
	err := NewMod().Init(cfg)
	if err == nil {
		t.Fatal("saga Mod started although dataengine.effects.max_age (800h) outlives saga.completion_receipt_ttl (720h)")
	}
	for _, want := range []string{"dataengine.effects.max_age", "saga.completion_receipt_ttl", "800h", "720h"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}

func TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom(t *testing.T) {
	for name, text := range map[string]string{
		"defaults (7d effects, 30d receipts)": "",
		"equal stream, receipts outlive":      "dataengine:\n  effects:\n    max_age: 200h\nsaga:\n  completion_receipt_ttl: 720h\n",
		// 结果效果流不是 DataEngine 的效果流：那条流的保留期由别处管，这里不比较。
		"results on another stream": "dataengine:\n  effects:\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n  result_effect_stream: OTHER_EFFECTS\n",
		// DataEngine 改了流名，saga 跟着改：仍是同一条流，要比较——这里 ttl 更长，通过。
		"renamed stream on both sides": "dataengine:\n  effects:\n    stream: GAME_EFFECTS\n    max_age: 100h\nsaga:\n  start_effect_stream: GAME_EFFECTS\n",
	} {
		if err := NewMod().Init(yamlConfig(t, text)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	renamed := "dataengine:\n  effects:\n    stream: GAME_EFFECTS\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n  start_effect_stream: GAME_EFFECTS\n"
	if err := NewMod().Init(yamlConfig(t, renamed)); err == nil || !strings.Contains(err.Error(), "dataengine.effects.max_age") {
		t.Fatalf("renamed shared stream with 800h retention: %v; want the cross-Mod check to refuse", err)
	}
}
