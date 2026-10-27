// sagajs 是原型的进程入口，给 kill -9 用例用：-role coord 起协调器（等租约过期后接管全部分区），
// -role part 起参与方（从 WAL 重建后开始消费，并在 sagajs_admin.<prefix>.balances（流外的 subject）上回答余额查询）。
// 就绪后在标准输出打一行 "ready ..."。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	jslogproto "github.com/tjbdwanghaibo/roost-core/scripts/perf/saga-jslog-proto"
)

func main() {
	role := flag.String("role", "", "coord | part")
	url := flag.String("url", "", "NATS URLs")
	prefix := flag.String("prefix", "", "subject prefix")
	parts := flag.Int("parts", 1, "partitions")
	instance := flag.String("instance", "", "coordinator instance name")
	steal := flag.Bool("steal", false, "take partitions without waiting for the lease to expire")
	leaseTTL := flag.Duration("lease-ttl", 3*time.Second, "owner lease TTL (must match the bucket)")
	snapshotEvery := flag.Int("snapshot-every", 0, "events between snapshots (0 = off)")
	walDir := flag.String("wal", "", "participant WAL directory")
	accounts := flag.Int("accounts", 64, "participant accounts")
	workers := flag.Int("workers", 16, "participant workers")
	durability := flag.String("durability", "strict", "strict | pipelined")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, err := nats.Connect(*url, nats.MaxReconnects(-1))
	if err != nil {
		fail(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		fail(err)
	}
	l := jslogproto.Layout{Prefix: *prefix, Partitions: *parts}
	ctx := context.Background()
	switch *role {
	case "coord":
		c := jslogproto.NewCoordinator(nc, js, jslogproto.CoordinatorOptions{Layout: l, Instance: *instance, LeaseTTL: *leaseTTL,
			SnapshotEvery: *snapshotEvery, PurgeGuards: []string{"part_acct"}, Log: log})
		begin := time.Now()
		for k := 0; k < *parts; k++ {
			for {
				err := c.Takeover(ctx, k, *steal)
				if err == nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
		fmt.Printf("ready coord %s replayed=%d in %v\n", *instance, c.Stats.ReplayMsgs.Load(), time.Since(begin))
	case "part":
		d := nest.DurabilityStrict
		if *durability == "pipelined" {
			d = nest.DurabilityPipelined
		}
		p, err := jslogproto.NewParticipant(nc, js, jslogproto.ParticipantOptions{Layout: l, WALDir: *walDir, Accounts: *accounts,
			InitialBal: 1_000_000, Workers: *workers, Durability: d, Log: log})
		if err != nil {
			fail(err)
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
			fail(err)
		}
		if err := p.Start(ctx); err != nil {
			fail(err)
		}
		fmt.Printf("ready part rebuilt_versions=%d in %v\n", p.RebuiltRecords, p.RebuildTime)
	default:
		fail(fmt.Errorf("unknown role %q", *role))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sagajs:", err)
	os.Exit(1)
}
