package mongo

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// 连接与凭据从环境变量读（R11，与 cube fork 对齐；规则在 wiring/mods 的 EnvValues，见 docs/framework/guide/13-wiring.md §2.1）：
// `mongo.<键>_env` 写了变量名，变量的值取代 `mongo.<键>`；变量没设置或为空时 Init 拒绝，错误点名键与变量名、不含任何值。

func mongoConfig(t *testing.T, body string) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMongoURIComesFromTheNamedEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_MONGO_URI", "mongodb://env-host:27017/?replicaSet=rs0")
	// 配置里的 mongo.uri 留作本地缺省，部署时由变量覆盖（下游 eks 配置两者都写）。
	mod := NewMongoMod()
	if err := mod.Init(mongoConfig(t, "mongo:\n  uri: mongodb://yaml-host:27017\n  uri_env: ROOST_TEST_MONGO_URI\n")); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.URI != "mongodb://env-host:27017/?replicaSet=rs0" {
		t.Fatalf("mongo.uri = %q, want the value of ROOST_TEST_MONGO_URI", mod.cfg.URI)
	}
	// `uri_env: ""`（叠加层显式清掉）时取 mongo.uri。
	mod = NewMongoMod()
	if err := mod.Init(mongoConfig(t, "mongo:\n  uri: mongodb://yaml-host:27017\n  uri_env: \"\"\n")); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.URI != "mongodb://yaml-host:27017" {
		t.Fatalf("mongo.uri = %q with an empty uri_env, want the configured value", mod.cfg.URI)
	}
}

func TestMongoInitRefusesADeclaredButMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_MONGO_EMPTY", "")
	t.Setenv("ROOST_TEST_MONGO_BLANK", "  ")
	t.Setenv("ROOST_TEST_MONGO_USER", "roost")
	t.Setenv("ROOST_TEST_MONGO_PASSWORD", "s3cret-pw")
	for _, key := range []string{"uri_env", "username_env", "password_env"} {
		for _, variable := range []string{"ROOST_TEST_MONGO_EMPTY", "ROOST_TEST_MONGO_BLANK", "ROOST_TEST_MONGO_UNSET_XYZ"} {
			cfg := viper.New()
			cfg.Set("mongo.username_env", "ROOST_TEST_MONGO_USER")
			cfg.Set("mongo.password_env", "ROOST_TEST_MONGO_PASSWORD")
			cfg.Set("mongo."+key, variable)
			err := NewMongoMod().Init(cfg)
			if err == nil || !strings.Contains(err.Error(), "mongo."+key) || !strings.Contains(err.Error(), variable) {
				t.Fatalf("mongo.%s: %s: Init = %v; want a refusal naming mongo.%s and %s", key, variable, err, key, variable)
			}
			if strings.Contains(err.Error(), "s3cret-pw") {
				t.Fatalf("the refusal carries a credential: %v", err)
			}
		}
	}
}

// 账号单独写（下游的写法：mongo.username_env / password_env 加 auth_source: admin）时进连接配置；规则与 cube 相同并补上与
// URI 的关系：用户名与口令同时写；URI 已带 userinfo 时不能再写用户名（两处都有账号，驱动会静默用后者）；auth_source 只跟单独
// 写的用户名用。拒绝的错误不带 URI 与口令。
func TestMongoCredentialsComeFromTheEnvironmentAndMustNotBeAmbiguous(t *testing.T) {
	t.Setenv("ROOST_TEST_MONGO_URI", "mongodb://db1:27017,db2:27017/?replicaSet=rs0")
	t.Setenv("ROOST_TEST_MONGO_USER", "roost")
	t.Setenv("ROOST_TEST_MONGO_PASSWORD", "s3cret-pw")
	mod := NewMongoMod()
	if err := mod.Init(mongoConfig(t, "mongo:\n  uri_env: ROOST_TEST_MONGO_URI\n  username_env: ROOST_TEST_MONGO_USER\n  password_env: ROOST_TEST_MONGO_PASSWORD\n  auth_source: admin\n")); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.Username != "roost" || mod.cfg.Password != "s3cret-pw" || mod.cfg.AuthSource != "admin" {
		t.Fatalf("username = %q, password from the variable = %v, auth_source = %q", mod.cfg.Username, mod.cfg.Password == "s3cret-pw", mod.cfg.AuthSource)
	}

	t.Setenv("ROOST_TEST_MONGO_URI_WITH_USER", "mongodb://other:s3cret-uri-pw@db1:27017/?replicaSet=rs0")
	for name, tc := range map[string]struct{ body, want string }{
		"username without password":    {"mongo:\n  username_env: ROOST_TEST_MONGO_USER\n", "mongo.username and mongo.password must be set together"},
		"password without username":    {"mongo:\n  password_env: ROOST_TEST_MONGO_PASSWORD\n", "mongo.username and mongo.password must be set together"},
		"credentials in both places":   {"mongo:\n  uri_env: ROOST_TEST_MONGO_URI_WITH_USER\n  username_env: ROOST_TEST_MONGO_USER\n  password_env: ROOST_TEST_MONGO_PASSWORD\n", "mongo.uri already carries credentials"},
		"auth_source without username": {"mongo:\n  uri_env: ROOST_TEST_MONGO_URI_WITH_USER\n  auth_source: admin\n", "mongo.auth_source applies to mongo.username"},
	} {
		err := NewMongoMod().Init(mongoConfig(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Init = %v; want %q", name, err, tc.want)
		}
		for _, secret := range []string{"s3cret-pw", "s3cret-uri-pw", "db1"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: the refusal carries %q: %v", name, secret, err)
			}
		}
	}
	// 只在 URI 里写账号（不写 mongo.username）照旧可用，auth_source 写在 URI 里。
	t.Setenv("ROOST_TEST_MONGO_URI_WITH_USER", "mongodb://other:s3cret-uri-pw@db1:27017/?replicaSet=rs0&authSource=admin")
	if err := NewMongoMod().Init(mongoConfig(t, "mongo:\n  uri_env: ROOST_TEST_MONGO_URI_WITH_USER\n")); err != nil {
		t.Fatalf("credentials only in the URI: %v", err)
	}
}

// 变量里的连接串带口令时，Start 的 “connected” 日志同样只记脱敏后的 URI；单独写的口令不进日志。
func TestStartDoesNotLogCredentialsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("ROOST_TEST_MONGO_URI", "mongodb://roost:s3cret-uri-pw@db1:27017/?replicaSet=rs0")
	t.Setenv("ROOST_TEST_MONGO_PASSWORD", "s3cret-pw")
	for _, body := range []string{
		"mongo:\n  uri_env: ROOST_TEST_MONGO_URI\n",
		"mongo:\n  uri: mongodb://db1:27017\n  username: roost\n  password_env: ROOST_TEST_MONGO_PASSWORD\n",
	} {
		mod := NewMongoMod()
		if err := mod.Init(mongoConfig(t, body)); err != nil {
			t.Fatal(err)
		}
		out := &lockedBuffer{}
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))
		mod.client = pingOnlyMongo{}
		err := mod.Start()
		slog.SetDefault(previous)
		if err != nil {
			t.Fatal(err)
		}
		if logged := out.String(); !strings.Contains(logged, "db1:27017") || strings.Contains(logged, "s3cret") {
			t.Fatalf("connected log = %q; want the host without any credential", logged)
		}
	}
}
