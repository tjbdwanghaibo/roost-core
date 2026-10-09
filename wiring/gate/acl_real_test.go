//go:build integration

package gate_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
)

// 私有子进程验证发布者权限，不把 payload/subject 自报身份当服务鉴权。
// 测试密码仅存在于临时隔离 broker，生产凭据与 singleton token 由部署注入。
func TestRealNATSACLRejectsForgedSourceAndForeignInbox(t *testing.T) {
	executable, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server executable unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	config := fmt.Sprintf(`listen: %q
 authorization { users: [
 {user: gate11, password: fixture, permissions: {
 publish: ["access.game.*.*.from.gate.11.gate000000000001.control", "access.game.*.*.from.gate.11.gate000000000001.forward"]
 subscribe: ["access.gate.11.gate000000000001.from.game.*.*.>", "access.reply.gate.11.gate000000000001.*"]
 allow_responses: {max: 1, expires: 2s}
 }},
 {user: game22, password: fixture, permissions: {
 publish: ["access.gate.*.*.from.game.22.game000000000001.>"]
 subscribe: ["access.game.22.game000000000001.from.gate.*.*.>", "access.reply.game.22.game000000000001.*"]
 allow_responses: {max: 1, expires: 2s}
 }}
 ]}
`, addr)
	root := t.TempDir()
	path := filepath.Join(root, "nats.conf")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "nats.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-c", path)
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	var game *gonats.Conn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		game, err = gonats.Connect("nats://"+addr, gonats.UserInfo("game22", "fixture"), gonats.NoReconnect(), gonats.Timeout(100*time.Millisecond))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("private ACL broker not ready: %v", err)
	}
	defer game.Close()
	violations := make(chan error, 8)
	gate, err := gonats.Connect("nats://"+addr, gonats.UserInfo("gate11", "fixture"), gonats.NoReconnect(), gonats.ErrorHandler(func(_ *gonats.Conn, _ *gonats.Subscription, err error) { violations <- err }))
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	subject := "access.game.22.game000000000001.from.gate.11.gate000000000001.forward"
	received := make(chan *gonats.Msg, 4)
	sub, err := game.Subscribe(subject, func(msg *gonats.Msg) { received <- msg; _ = msg.Respond([]byte("ack")) })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := game.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	reply := "access.reply.gate.11.gate000000000001.unique"
	replies, err := gate.SubscribeSync(reply)
	if err != nil {
		t.Fatal(err)
	}
	defer replies.Unsubscribe()
	if err := gate.PublishRequest(subject, reply, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	if _, err := replies.NextMsg(time.Second); err != nil {
		t.Fatalf("valid source/reply rejected: %v", err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("valid request not delivered")
	}
	expectDenied := func(action func() error) {
		t.Helper()
		if err := action(); err != nil {
			t.Fatal(err)
		}
		_ = gate.FlushTimeout(time.Second)
		select {
		case err := <-violations:
			if !strings.Contains(err.Error(), "Permissions Violation") {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("unauthorized operation was not rejected")
		}
	}
	expectDenied(func() error {
		return gate.Publish("access.game.22.game000000000001.from.gate.12.gate000000000002.forward", []byte("forged"))
	})
	expectDenied(func() error {
		return gate.Publish("access.gate.11.gate000000000001.from.game.22.game000000000001.outbound", []byte("forged game"))
	})
	expectDenied(func() error { _, err := gate.SubscribeSync("access.reply.game.22.game000000000001.*"); return err })
	expectDenied(func() error {
		return gate.Publish("access.reply.game.22.game000000000001.unsolicited", []byte("forged reply"))
	})
	select {
	case msg := <-received:
		t.Fatalf("forged request reached game: %s", msg.Subject)
	case <-time.After(20 * time.Millisecond):
	}
}
