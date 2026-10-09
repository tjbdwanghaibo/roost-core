package driver

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// O-M6-3（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md）：EvalReplicated 的契约。
//
//   - WAIT 与脚本在同一条连接上（WAIT 只统计本连接之前的写）；
//   - 主节点没有连着的副本时不发 WAIT（单机开发环境不被卡到超时）；
//   - 副本确认不足不是错误；ROLE / WAIT 出错时脚本结果照常返回，错误只在 WaitErr；
//   - 脚本自己的错误照 Eval 的分类返回，不发 WAIT；
//   - numReplicas / timeout 不为正时普通发送脚本。

// recordingRESPServer 与 cannedRESPServer 相同，另按连接记录收到的命令名。
func recordingRESPServer(t *testing.T, replies map[string]string) (*goredis.Client, func() [][]string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var conns [][]string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			index := len(conns)
			conns = append(conns, nil)
			mu.Unlock()
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					args, err := readRESPCommand(reader)
					if err != nil {
						return
					}
					name := strings.ToUpper(args[0])
					if name != "HELLO" { // 连接握手，不计
						mu.Lock()
						conns[index] = append(conns[index], name)
						mu.Unlock()
					}
					reply, ok := replies[name]
					if !ok {
						reply = "-ERR unknown command '" + args[0] + "'\r\n"
					}
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	client := goredis.NewClient(&goredis.Options{
		Addr: listener.Addr().String(), Protocol: 2, DisableIdentity: true,
		MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]string, len(conns))
		for i, c := range conns {
			out[i] = append([]string(nil), c...)
		}
		return out
	}
}

const (
	roleOneReplica = "*3\r\n$6\r\nmaster\r\n:100\r\n*1\r\n*3\r\n$9\r\n127.0.0.1\r\n$4\r\n6380\r\n$3\r\n100\r\n"
	roleNoReplica  = "*3\r\n$6\r\nmaster\r\n:100\r\n*0\r\n"
)

// connectionWith 返回含 command 的那条连接上的命令序列。
func connectionWith(conns [][]string, command string) []string {
	for _, c := range conns {
		for _, name := range c {
			if name == command {
				return c
			}
		}
	}
	return nil
}

func TestEvalReplicatedWaitsOnTheScriptsConnection(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		role     string
		wait     string
		waited   bool
		replicas int64
		skipped  string
		waitErr  bool
	}{
		{name: "confirmed", role: roleOneReplica, wait: ":1\r\n", waited: true, replicas: 1},
		{name: "short", role: roleOneReplica, wait: ":0\r\n", waited: true, replicas: 0},
		{name: "no replicas", role: roleNoReplica, skipped: fredis.ReplicatedSkipNoReplicas},
		{name: "wait error", role: roleOneReplica, wait: "-ERR wait failed\r\n", waitErr: true},
		{name: "not a master", role: "*5\r\n$5\r\nslave\r\n$9\r\n127.0.0.1\r\n:6379\r\n$9\r\nconnected\r\n:10\r\n", waitErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]string{"EVAL": ":1\r\n", "ROLE": tc.role}
			if tc.wait != "" {
				replies["WAIT"] = tc.wait
			}
			rdb, commands := recordingRESPServer(t, replies)
			client := &Client{rdb: rdb}
			got, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 1, 50*time.Millisecond)
			if err != nil {
				t.Fatalf("script error %v; the script succeeded", err)
			}
			if got.Result != int64(1) || got.Waited != tc.waited || got.Replicas != tc.replicas || got.Skipped != tc.skipped || (got.WaitErr != nil) != tc.waitErr {
				t.Fatalf("result=%+v, want waited=%v replicas=%d skipped=%q waitErr=%v", got, tc.waited, tc.replicas, tc.skipped, tc.waitErr)
			}
			sequence := connectionWith(commands(), "EVAL")
			wantWait := tc.wait != ""
			if wantWait && strings.Join(sequence, ",") != "EVAL,ROLE,WAIT" {
				t.Fatalf("commands on the script's connection = %v, want EVAL,ROLE,WAIT", sequence)
			}
			if !wantWait && strings.Join(sequence, ",") != "EVAL,ROLE" {
				t.Fatalf("commands on the script's connection = %v, want EVAL,ROLE (no WAIT)", sequence)
			}
		})
	}
}

func TestEvalReplicatedScriptErrorsAndDisabledWait(t *testing.T) {
	ctx := context.Background()
	rdb, commands := recordingRESPServer(t, map[string]string{"EVAL": "-ERR user_script failed\r\n", "ROLE": roleOneReplica, "WAIT": ":1\r\n"})
	client := &Client{rdb: rdb}
	if _, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 1, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "user_script") {
		t.Fatalf("script error = %v, want the script's own error", err)
	}
	if seq := connectionWith(commands(), "WAIT"); seq != nil {
		t.Fatalf("WAIT was sent after a failed script: %v", seq)
	}

	rdb, commands = recordingRESPServer(t, map[string]string{"EVAL": ":1\r\n"})
	client = &Client{rdb: rdb}
	got, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 0, 50*time.Millisecond)
	if err != nil || got.Result != int64(1) || got.Skipped != fredis.ReplicatedSkipDisabled || got.Waited {
		t.Fatalf("disabled: result=%+v err=%v", got, err)
	}
	if seq := connectionWith(commands(), "EVAL"); strings.Join(seq, ",") != "EVAL" {
		t.Fatalf("disabled sent %v, want only EVAL", seq)
	}
}
