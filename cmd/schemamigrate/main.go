// Command schemamigrate 是 Player schema 1 → 2 的离线迁移工具（db/migrations）。
//
//	go run ./cmd/schemamigrate -mongo "$URI" plan       # dry-run：转换并校验全部文档，不写
//	go run ./cmd/schemamigrate -mongo "$URI" apply      # 先 plan，全部通过才逐份“备份 → 条件改写”
//	go run ./cmd/schemamigrate -mongo "$URI" verify     # 切换版本前的验收：无旧版本文档、全部能按当前 schema 解码
//	go run ./cmd/schemamigrate -mongo "$URI" rollback   # 新程序写入前，用备份原样恢复
//
// 只能在停写之后运行：所有写 Player 集合的 game 进程已经正常停机（DataEngine 停机把 WAL 全部投影
// 进 Mongo），新程序还没有启动。完整步骤与回退边界见 roost-core 的 docs/framework/SCHEMA-MIGRATION.md。
// 每个子命令打印一份 JSON 报告；失败时以非零码退出。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	db "example.com/planet/db"
	"example.com/planet/db/migrations"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

func main() {
	uri := flag.String("mongo", "mongodb://127.0.0.1:27017/?replicaSet=rs0", "the Mongo the game process writes Player documents to (mongo.uri in config.game.yaml)")
	database := flag.String("db", db.PlayerDaoDBName, "database of the Player collection (the DAO marker's db=)")
	collection := flag.String("coll", db.PlayerDaoCollection, "the Player collection")
	backup := flag.String("backup", db.PlayerDaoCollection+"_schema1_backup", "collection that keeps each rewritten document as it was, for rollback")
	timeout := flag.Duration("timeout", 30*time.Minute, "upper bound for the whole run")
	flag.Parse()
	if flag.NArg() != 1 {
		fail("usage: schemamigrate [-mongo uri] [-db name] [-coll name] [-backup name] plan|apply|verify|rollback")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	client, err := mongodriver.NewClient(fmongo.DefaultConfig(*uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		fail("mongo: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := client.Ping(ctx); err != nil {
		fail("mongo: %v", err)
	}

	steps, err := migrations.PlayerSteps()
	if err != nil {
		fail("steps: %v", err)
	}
	job := migrations.PlayerJob(steps)
	job.Collection = client.Database(*database).Collection(*collection)
	job.Backup = client.Database(*database).Collection(*backup)

	var report migrations.Report
	switch flag.Arg(0) {
	case "plan":
		report, err = job.Plan(ctx)
	case "apply":
		report, err = job.Apply(ctx)
	case "verify":
		report, err = job.Verify(ctx)
	case "rollback":
		report, err = job.Rollback(ctx)
	default:
		fail("unknown command %q (commands: plan, apply, verify, rollback)", flag.Arg(0))
	}
	report.Collection = *database + "." + *collection
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
	if err != nil {
		fail("%s: %v", flag.Arg(0), err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "schemamigrate: "+format+"\n", args...)
	os.Exit(1)
}
