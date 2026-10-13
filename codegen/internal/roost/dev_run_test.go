package roost

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// deploy/dev/run.sh 向 account 服务登记游戏服时，必须连到 account 服务自己的那份 Redis：config.account.yaml 的
// redis.addr、redis.password、redis.db 都传给 accountctl（只读 addr 时非 0 db 的登记写进 db 0，CreateRole 报 server is invalid）。

// registerGameServerArgs runs the generated register_game_server function
// against configs/service/config.account.yaml = accountConfig, with a stub go
// on PATH that records its arguments, and returns them one per line.
func registerGameServerArgs(t *testing.T, accountConfig string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub go is a POSIX shell script; the generated dev scripts target POSIX sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	script := renderDevRun(DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil))
	start := strings.Index(script, "register_game_server() {\n")
	if start < 0 {
		t.Fatal("deploy/dev/run.sh has no register_game_server function")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatal("register_game_server is not terminated")
	}
	function := script[start : start+end+len("\n}\n")]

	dir := t.TempDir()
	for _, sub := range []string{"cmd/accountctl", "configs/service", "stub"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(rel, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("configs/service/config.account.yaml", accountConfig, 0o644)
	writeFile("stub/go", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\n", 0o755)
	writeFile("register.sh", "set -eu\nSID=1000\n"+function+"register_game_server\n", 0o644)

	argsFile := filepath.Join(dir, "args")
	cmd := exec.Command("sh", "register.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "stub")+string(os.PathListSeparator)+os.Getenv("PATH"), "ARGS_FILE="+argsFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("register_game_server: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("register_game_server did not run accountctl: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// flagValue is the argument after name, or "" with ok false.
func flagValue(args []string, name string) (string, bool) {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1], true
		}
	}
	return "", false
}

func TestDevRunRegistersTheGameServerInTheAccountServicesRedisDatabase(t *testing.T) {
	args := registerGameServerArgs(t, `redis:
  addr: 10.0.0.5:6380
  password: "s3cret pass"
  db: 13 # the account service's own database
  pool_size: 32
nats:
  url: nats://127.0.0.1:4222
account:
  key_prefix: roost:planet:account
  db: 99
`)
	for _, want := range []struct{ flag, value string }{
		{"-redis", "10.0.0.5:6380"},
		{"-redis-password", "s3cret pass"},
		{"-redis-db", "13"},
		{"-prefix", "roost:planet:account"},
	} {
		if got, ok := flagValue(args, want.flag); !ok || got != want.value {
			t.Errorf("accountctl %s = %q (present %v), want %q; args %q", want.flag, got, ok, want.value, args)
		}
	}
}

// The generated config (password "", db 0) and one without the keys both keep
// accountctl's defaults: db 0, no password.
func TestDevRunKeepsAccountctlDefaultsWhenTheRedisKeysAreAbsent(t *testing.T) {
	for name, config := range map[string]string{
		"generated values": "redis:\n  addr: 127.0.0.1:6379\n  password: \"\"\n  db: 0\naccount:\n  key_prefix: roost:planet:account\n",
		"keys absent":      "redis:\n  addr: 127.0.0.1:6379\naccount:\n  key_prefix: roost:planet:account\n",
	} {
		t.Run(name, func(t *testing.T) {
			args := registerGameServerArgs(t, config)
			if got, ok := flagValue(args, "-redis-db"); ok && got != "0" {
				t.Errorf("accountctl -redis-db = %q, want 0 or absent; args %q", got, args)
			}
			if got, ok := flagValue(args, "-redis-password"); ok && got != "" {
				t.Errorf("accountctl -redis-password = %q, want empty or absent; args %q", got, args)
			}
		})
	}
}

// account 服务配成 Redis Cluster（redis.cluster_addrs，逗号串或 YAML 列表）时，登记游戏服也要连这个 Cluster，
// 不能退回 127.0.0.1:6379 或用单机客户端写某一个节点（槽不在该节点的键会 MOVED）。
func TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster(t *testing.T) {
	for name, config := range map[string]string{
		"comma string": "redis:\n  cluster_addrs: \"10.0.0.1:7000, 10.0.0.2:7000,10.0.0.3:7000\"\n  password: \"\"\naccount:\n  key_prefix: roost:planet:account\n",
		"yaml list":    "redis:\n  cluster_addrs:\n    - 10.0.0.1:7000\n    - \"10.0.0.2:7000\"\n    - 10.0.0.3:7000\n  password: \"\"\naccount:\n  key_prefix: roost:planet:account\n",
	} {
		t.Run(name, func(t *testing.T) {
			args := registerGameServerArgs(t, config)
			if got, ok := flagValue(args, "-redis-cluster"); !ok || got != "10.0.0.1:7000,10.0.0.2:7000,10.0.0.3:7000" {
				t.Errorf("accountctl -redis-cluster = %q (present %v), want the three seeds; args %q", got, ok, args)
			}
			if got, ok := flagValue(args, "-prefix"); !ok || got != "roost:planet:account" {
				t.Errorf("accountctl -prefix = %q, want roost:planet:account; args %q", got, args)
			}
		})
	}
}
