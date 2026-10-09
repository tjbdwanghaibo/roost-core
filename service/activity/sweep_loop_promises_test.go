package activity

import (
	"context"
	"testing"
	"time"
)

// U-0119 · 配置无执行者（与 U-0022 同形态）。
//
// 宽限窗口是这个服务唯一能在游戏服沉默时收尾一次聚合的机制，而它只由后台
// sweep 调用 AdvanceExpired 来兑现。此前 Server.sweepGroups 是一个返回 nil 的
// 桩、没有任何配置入口：没有一个进程在后台推进过任何组。这里配置了组，就
// 要求循环在窗口过期后把聚合推到 complete；没配置组则什么都不扫、干净退出。

func TestTheSweepLoopAdvancesTheConfiguredGroups(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t, func(cfg *Config) { cfg.SweepGroups = []string{"group-a"} })
	key := activityKey("act-loop")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1) // deadline = now + 30s grace
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		activity, found, err := service.LookupActivity(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if found && activity.Status == StatusComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep loop never advanced the lapsed activity in a configured group: status=%v", activity.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTheSweepLoopWithoutGroupsSweepsNothingAndStopsCleanly(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t)
	key := activityKey("act-idle")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if activity, _, _ := service.LookupActivity(context.Background(), key); activity.Status != StatusCollecting {
		t.Fatalf("a process with no configured groups advanced an activity: %v", activity.Status)
	}
}
