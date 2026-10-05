package mongo

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
)

// RR-20261005-NC-191：Mongo 的账号口令只能写在 mongo.uri 的 userinfo 里（fmongo.Config 没有单独的
// 用户名 / 密码字段），Start 的 “connected” 日志不能把它写进日志。旧行为：
// `slog.Info("mongo mod: connected", "uri", m.cfg.URI)` 原样记录整条 URI，口令进了日志文件。
//
// 用不拨号的替身：Start 只调 Ping，类型断言不到 *mongodriver.Client 时不做部署校验，正好走到那行日志。

type pingOnlyMongo struct {
	fmongo.IMongo
}

func (pingOnlyMongo) Ping(context.Context) error { return nil }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStartDoesNotLogTheMongoPassword(t *testing.T) {
	for _, uri := range []string{
		"mongodb://roost:s3cret-pw@db1:27017,db2:27017/?replicaSet=rs0",
		"mongodb+srv://roost:s3cret-pw@cluster0.example.net/game?retryWrites=true",
		"mongodb://roost:s3cret-pw@db1:27017",
	} {
		out := &lockedBuffer{}
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
		mod := &MongoMod{client: pingOnlyMongo{}, cfg: fmongo.DefaultConfig(uri)}
		err := mod.Start()
		slog.SetDefault(previous)
		if err != nil {
			t.Fatalf("Start(%s): %v", uri, err)
		}
		logged := out.String()
		if !strings.Contains(logged, "mongo mod: connected") {
			t.Fatalf("Start did not log the connection: %q", logged)
		}
		if strings.Contains(logged, "s3cret-pw") {
			t.Fatalf("Start logged the Mongo password: %q", logged)
		}
		// 主机、用户名与选项仍然可见，排障用得上。
		if !strings.Contains(logged, "roost") || (!strings.Contains(logged, "db1:27017") && !strings.Contains(logged, "cluster0.example.net")) {
			t.Fatalf("Start no longer logs where it connected: %q", logged)
		}
	}
}

func TestRedactedURIKeepsEverythingButThePassword(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"mongodb://roost:pw@db1:27017,db2:27017/game?replicaSet=rs0", "mongodb://roost:***@db1:27017,db2:27017/game?replicaSet=rs0"},
		{"mongodb+srv://roost:p%40ss@cluster0.example.net/", "mongodb+srv://roost:***@cluster0.example.net/"},
		// 口令里没转义的 '/'：仍按 '?' 前最后一个 '@' 切开。
		{"mongodb://roost:a/b@db1:27017", "mongodb://roost:***@db1:27017"},
		// 选项里的 '@' 不当作 userinfo 的结尾。
		{"mongodb://roost:pw@db1/?authMechanismProperties=SERVICE_NAME:a@b", "mongodb://roost:***@db1/?authMechanismProperties=SERVICE_NAME:a@b"},
		{"mongodb://roost@db1:27017", "mongodb://roost@db1:27017"},
		{"mongodb://127.0.0.1:27017/?replicaSet=rs0", "mongodb://127.0.0.1:27017/?replicaSet=rs0"},
		{"not a uri", "not a uri"},
	} {
		if got := redactedURI(tc.in); got != tc.want {
			t.Errorf("redactedURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
