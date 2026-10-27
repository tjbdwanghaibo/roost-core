package jslogproto

// kill -9 用例的进程入口：测试二进制带 SAGAJS_ROLE_ARGS 启动时不跑测试，按参数起协调器或参与方。
// 放在测试文件里（而不是 cmd/ 下的 main）：要连 Mongo 驱动，而根包门禁只许测试代码与 wiring 链接驱动。
//   -role coord：等租约过期后接管全部分区；-project 时同进程跑 saga 冷副本投影。
//   -role part：从 WAL（投影关闭）或 Mongo + WAL 尾巴（投影打开）恢复后开始消费，并在 sagajs_admin.<prefix>.balances
//   （流外的 subject）上回答余额查询。
// 就绪后在标准输出打一行 "ready ..."。Mongo 地址只经环境变量 SAGAJS_MONGO_URI 传入，不打印。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

const roleArgsEnv = "SAGAJS_ROLE_ARGS"

func TestMain(m *testing.M) {
	if raw := os.Getenv(roleArgsEnv); raw != "" {
		os.Exit(roleMain(strings.Split(raw, "\x1f")))
	}
	os.Exit(m.Run())
}

func newMongo(uri string) (fmongo.IMongo, error) {
	return driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
}

func roleMain(args []string) int {
	fs := flag.NewFlagSet("sagajs", flag.ContinueOnError)
	role := fs.String("role", "", "coord | part")
	url := fs.String("url", "", "NATS URLs")
	prefix := fs.String("prefix", "", "subject prefix")
	parts := fs.Int("parts", 1, "partitions")
	groups := fs.Int("stream-groups", 0, "streams the partitions are spread over (0 = default)")
	instance := fs.String("instance", "", "coordinator instance name")
	steal := fs.Bool("steal", false, "take partitions without waiting for the lease to expire")
	leaseTTL := fs.Duration("lease-ttl", 3*time.Second, "owner lease TTL (must match the bucket)")
	walDir := fs.String("wal", "", "participant WAL directory")
	accounts := fs.Int("accounts", 64, "participant accounts")
	workers := fs.Int("workers", 16, "participant workers")
	durability := fs.String("durability", "strict", "strict | pipelined")
	project := fs.Bool("project", false, "Mongo projection on (SAGAJS_MONGO_URI)")
	database := fs.String("db", "", "unique Mongo database")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "sagajs:", err); return 1 }
	nc, err := nats.Connect(*url, nats.MaxReconnects(-1))
	if err != nil {
		return fail(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return fail(err)
	}
	var mongo fmongo.IMongo
	if *project {
		if mongo, err = newMongo(os.Getenv("SAGAJS_MONGO_URI")); err != nil {
			return fail(err)
		}
	}
	l := Layout{Prefix: *prefix, Partitions: *parts, StreamGroups: *groups}
	ctx := context.Background()
	switch *role {
	case "coord":
		guards := []string{"part_acct"}
		if mongo != nil {
			sp := NewSagaProjector(js, l, mongo, *database, log)
			if err := sp.Start(ctx); err != nil {
				return fail(err)
			}
			guards = append(guards, SagaProjectionDurable)
		}
		c := NewCoordinator(nc, js, CoordinatorOptions{Layout: l, Instance: *instance, LeaseTTL: *leaseTTL, PurgeGuards: guards, Log: log})
		begin := time.Now()
		for k := 0; k < *parts; k++ {
			for c.Takeover(ctx, k, *steal) != nil {
				time.Sleep(200 * time.Millisecond)
			}
		}
		fmt.Printf("ready coord %s replayed=%d in %v\n", *instance, c.Stats.ReplayMsgs.Load(), time.Since(begin))
	case "part":
		d := nest.DurabilityStrict
		if *durability == "pipelined" {
			d = nest.DurabilityPipelined
		}
		p, err := NewParticipant(nc, js, ParticipantOptions{Layout: l, WALDir: *walDir, Accounts: *accounts, InitialBal: initialBalance,
			Workers: *workers, Durability: d, Log: log, Mongo: mongo, Database: *database})
		if err != nil {
			return fail(err)
		}
		if _, err := nc.Subscribe("sagajs_admin."+*prefix+".balances", func(m *nats.Msg) {
			out := map[int64]int64{}
			for n := int64(0); n < int64(*accounts); n++ {
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				b, err := p.Balance(rctx, n)
				cancel()
				if err != nil {
					_ = m.Respond([]byte(`{"error":"` + err.Error() + `"}`))
					return
				}
				out[n] = b
			}
			raw, _ := json.Marshal(out)
			_ = m.Respond(raw)
		}); err != nil {
			return fail(err)
		}
		if err := p.Start(ctx); err != nil {
			return fail(err)
		}
		fmt.Printf("ready part recovered_versions=%d in %v projection=%v\n", p.RebuiltRecords, p.RebuildTime, mongo != nil)
	default:
		return fail(fmt.Errorf("unknown role %q", *role))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return 0
}
